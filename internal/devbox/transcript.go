package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// TranscriptPath is where a dev box POSTs transcript batches.
const TranscriptPath = "/api/devboxes/transcripts"

// TranscriptCursorsPath is where a dev box reads which transcripts the server already has.
func TranscriptCursorsPath(box string) string { return "/api/devboxes/" + box + "/transcripts" }

// TranscriptBatch is part of one agent's transcript. Replace starts the transcript over (a first
// sync, or a new epoch); otherwise its entries replace those with the same SeqStart and are added
// after the rest.
type TranscriptBatch struct {
	Box     string            `json:"box"`
	AgentID string            `json:"agentId"`
	Epoch   string            `json:"epoch"`
	Replace bool              `json:"replace"`
	Entries []TranscriptEntry `json:"entries"`
}

// TranscriptEntry is one entry of an agent's projected Paseo timeline, with the daemon's item whole.
type TranscriptEntry struct {
	SeqStart int             `json:"seqStart"`
	SeqEnd   int             `json:"seqEnd"`
	TurnID   string          `json:"turnId,omitempty"`
	Provider string          `json:"provider,omitempty"`
	At       time.Time       `json:"at"`
	Item     json.RawMessage `json:"item"`
}

// TranscriptCursor is where the server's copy of a transcript ends.
type TranscriptCursor struct {
	Epoch        string `json:"epoch"`
	LastSeqStart int    `json:"lastSeqStart"`
	LastSeqEnd   int    `json:"lastSeqEnd"`
}

// TranscriptSync copies the transcripts of the box's loaded agents to the server. It reads each
// agent's timeline from the daemon (Timeline) only while the agent is loaded: reading a closed
// agent's timeline starts it again in Paseo. Not safe for concurrent use.
type TranscriptSync struct {
	Server, Token, Box string
	HTTP               *http.Client
	Timeline           func(ctx context.Context, agentID string, after *paseo.Cursor) (paseo.Timeline, error)
	MaxBatch           int // bytes per upload; default 4 MB

	cursors map[string]TranscriptCursor // the server's copy, by agent; read from the server once
	synced  map[string]time.Time        // each agent's UpdatedAt when it was last synced
}

// loaded statuses: the daemon holds these agents' timelines, so reading one starts nothing.
var loaded = map[string]bool{"running": true, "idle": true, "error": true, "initializing": true}

// Sync uploads what is new in each loaded agent's transcript. It returns the first failure; the
// other agents are still synced, and a failed agent is tried again on the next call.
func (s *TranscriptSync) Sync(ctx context.Context, agents []Agent) error {
	if s.cursors == nil {
		cur, err := s.readCursors(ctx)
		if err != nil {
			return fmt.Errorf("transcript cursors: %w", err)
		}
		s.cursors, s.synced = cur, map[string]time.Time{}
	}
	var first error
	for _, a := range agents {
		if !loaded[a.Status] {
			continue
		}
		if t, ok := s.synced[a.ID]; ok && t.Equal(a.UpdatedAt) && a.Status != "running" {
			continue
		}
		err := s.syncAgent(ctx, a.ID)
		if errors.Is(err, errNotYet) {
			continue // not a failure: its report has not landed; tried again next time
		}
		if err != nil {
			if first == nil {
				first = fmt.Errorf("transcript of %s: %w", a.ID, err)
			}
			continue
		}
		s.synced[a.ID] = a.UpdatedAt
	}
	return first
}

// errResend: the server cannot place an append (409); the transcript is sent whole.
var errResend = errors.New("server asked for the whole transcript")

// errNotYet: the server does not know the agent yet (404): its report has not landed.
var errNotYet = errors.New("agent not on the server yet")

func (s *TranscriptSync) syncAgent(ctx context.Context, id string) error {
	cur, have := s.cursors[id]
	var after *paseo.Cursor
	if have && cur.LastSeqStart > 0 {
		// from just before the last entry: a message still streaming grows in place
		after = &paseo.Cursor{Epoch: cur.Epoch, Seq: cur.LastSeqStart - 1}
	}
	tl, err := s.Timeline(ctx, id, after)
	if err != nil {
		return err
	}
	whole := !have || after == nil || tl.Reset
	if !whole && len(tl.Entries) == 1 && tl.Entries[0].SeqStart == cur.LastSeqStart && tl.Entries[0].SeqEnd == cur.LastSeqEnd {
		return nil // nothing new
	}
	err = s.upload(ctx, id, tl, whole)
	if errors.Is(err, errResend) && !whole {
		if tl, err = s.Timeline(ctx, id, nil); err != nil {
			return err
		}
		err = s.upload(ctx, id, tl, true)
	}
	return err
}

// upload sends tl's entries in batches of at most MaxBatch bytes; with whole, the first replaces
// the server's copy. It records the new cursor.
func (s *TranscriptSync) upload(ctx context.Context, id string, tl paseo.Timeline, whole bool) error {
	max := s.MaxBatch
	if max <= 0 {
		max = 4 << 20
	}
	batch := TranscriptBatch{Box: s.Box, AgentID: id, Epoch: tl.Epoch, Replace: whole}
	size := 0
	flush := func() error {
		if err := s.post(ctx, batch); err != nil {
			return err
		}
		batch.Replace, batch.Entries, size = false, nil, 0
		return nil
	}
	for _, e := range tl.Entries {
		te := TranscriptEntry{SeqStart: e.SeqStart, SeqEnd: e.SeqEnd, TurnID: e.TurnID, Provider: e.Provider, At: e.Timestamp, Item: e.Item}
		n := len(e.Item) + 200
		if len(batch.Entries) > 0 && size+n > max {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.Entries, size = append(batch.Entries, te), size+n
	}
	if len(batch.Entries) > 0 || batch.Replace {
		if err := flush(); err != nil {
			return err
		}
	}
	cur := TranscriptCursor{Epoch: tl.Epoch}
	if !whole {
		cur = s.cursors[id]
		cur.Epoch = tl.Epoch
	}
	if n := len(tl.Entries); n > 0 {
		cur.LastSeqStart, cur.LastSeqEnd = tl.Entries[n-1].SeqStart, tl.Entries[n-1].SeqEnd
	}
	s.cursors[id] = cur
	return nil
}

func (s *TranscriptSync) post(ctx context.Context, b TranscriptBatch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.Server, "/")+TranscriptPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		return errResend
	case resp.StatusCode == http.StatusNotFound:
		return errNotYet
	case resp.StatusCode/100 != 2:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload refused: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (s *TranscriptSync) readCursors(ctx context.Context) (map[string]TranscriptCursor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Server, "/")+TranscriptCursorsPath(s.Box), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	out := map[string]TranscriptCursor{}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
