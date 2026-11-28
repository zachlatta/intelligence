package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// TranscriptBatch is part of one agent's transcript. Replace starts the transcript over (a first
// sync, or a new epoch); otherwise its entries replace those with the same SeqStart and are added
// after the rest.
type TranscriptBatch struct {
	Host    string            `json:"host"`
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

type TranscriptCursor struct {
	Epoch        string `json:"epoch"`
	LastSeqStart int    `json:"lastSeqStart"`
	LastSeqEnd   int    `json:"lastSeqEnd"`
}

// TranscriptSync copies the transcripts of the host's loaded agents to the server. It reads each
// agent's timeline from the daemon (Timeline) only while the agent is loaded: reading a closed
// agent's timeline starts it again in Paseo. Not safe for concurrent use.
type TranscriptSync struct {
	Host     string
	Timeline func(ctx context.Context, agentID string, after *paseo.Cursor) (paseo.Timeline, error)
	// Cursors reads where the server's copies end (Link.TranscriptCursors), once per Reset.
	Cursors func(ctx context.Context) (map[string]TranscriptCursor, error)
	// Send stores a batch on the server (Link.Transcript): ErrResend and ErrNotYet are answers, not failures.
	Send func(ctx context.Context, b TranscriptBatch) error
	// OnFileEdit, when set, is called with the agentID whenever an uploaded entry indicates a file write/edit.
	OnFileEdit func(agentID string)
	MaxBatch   int // bytes per upload; default 4 MB

	cursors map[string]TranscriptCursor // the server's copy, by agent
	synced  map[string]time.Time        // each agent's UpdatedAt when it was last synced
}

// itemMayEditFiles reports whether a timeline item is a tool call that may have modified files
// in the workspace (edit, write, shell/bash, patch). Read/search/reasoning/messages return false.
func itemMayEditFiles(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var item struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Detail struct {
			Type string `json:"type"`
		} `json:"detail"`
	}
	if json.Unmarshal(raw, &item) != nil || item.Type != "tool_call" {
		return false
	}
	switch item.Detail.Type {
	case "edit", "write", "shell":
		return true
	case "read", "search":
		return false
	}
	switch item.Name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "Bash", "edit", "write", "bash", "shell", "apply_patch", "write_file", "edit_file":
		return true
	}
	return false
}

// PushStreamEvent forwards a contiguous agent_stream timeline event directly to the server
// without re-fetching the agent's full timeline from Paseo. It returns handled=false when the
// cursor is missing, the epoch differs, there is a seq gap, or the server asks for a resend,
// so the caller can fall back to a normal Sync gap-resync.
func (s *TranscriptSync) PushStreamEvent(ctx context.Context, ev paseo.TimelineStreamEvent) (bool, error) {
	if s.cursors == nil || ev.Epoch == "" || ev.Seq <= 0 || len(ev.Item) == 0 || !json.Valid(ev.Item) {
		return false, nil
	}
	cur, have := s.cursors[ev.AgentID]
	if !have || cur.Epoch != ev.Epoch {
		return false, nil
	}
	// Only direct-push contiguous next seq or in-place update of the current entry.
	var seqStart int
	switch {
	case ev.Seq == cur.LastSeqEnd+1:
		seqStart = ev.Seq
	case ev.Seq == cur.LastSeqStart && cur.LastSeqStart > 0:
		seqStart = cur.LastSeqStart
	default:
		return false, nil
	}
	entry := TranscriptEntry{
		SeqStart: seqStart,
		SeqEnd:   ev.Seq,
		TurnID:   ev.TurnID,
		Provider: ev.Provider,
		At:       ev.Timestamp,
		Item:     ev.Item,
	}
	err := s.Send(ctx, TranscriptBatch{
		Host:    s.Host,
		AgentID: ev.AgentID,
		Epoch:   ev.Epoch,
		Replace: false,
		Entries: []TranscriptEntry{entry},
	})
	if errors.Is(err, ErrResend) || errors.Is(err, ErrNotYet) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cur.LastSeqStart = seqStart
	cur.LastSeqEnd = ev.Seq
	s.cursors[ev.AgentID] = cur
	if s.OnFileEdit != nil && itemMayEditFiles(ev.Item) {
		s.OnFileEdit(ev.AgentID)
	}
	return true, nil
}

// Reset forgets what the server has, so the next Sync reads its cursors again (a new stream: the
// server may have changed meanwhile).
func (s *TranscriptSync) Reset() { s.cursors, s.synced = nil, nil }

// loaded statuses: the daemon holds these agents' timelines, so reading one starts nothing.
var loaded = map[string]bool{"running": true, "idle": true, "error": true, "initializing": true}

// Sync uploads what is new in each loaded agent's transcript. It returns the first failure; the
// other agents are still synced, and a failed agent is tried again on the next call.
func (s *TranscriptSync) Sync(ctx context.Context, agents []Agent) error {
	if s.cursors == nil {
		cur, err := s.Cursors(ctx)
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
		if errors.Is(err, ErrNotYet) {
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
	if errors.Is(err, ErrResend) && !whole {
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
	batch := TranscriptBatch{Host: s.Host, AgentID: id, Epoch: tl.Epoch, Replace: whole}
	size := 0
	flush := func() error {
		if err := s.Send(ctx, batch); err != nil {
			return err
		}
		batch.Replace, batch.Entries, size = false, nil, 0
		return nil
	}
	edited := false
	for _, e := range tl.Entries {
		te := TranscriptEntry{SeqStart: e.SeqStart, SeqEnd: e.SeqEnd, TurnID: e.TurnID, Provider: e.Provider, At: e.Timestamp, Item: e.Item}
		if !edited && s.OnFileEdit != nil && itemMayEditFiles(e.Item) {
			edited = true
		}
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
	if edited && s.OnFileEdit != nil {
		s.OnFileEdit(id)
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
