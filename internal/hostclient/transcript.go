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

	cursors   map[string]TranscriptCursor           // the server's copy, by agent
	synced    map[string]time.Time                  // each agent's UpdatedAt when it was last synced
	active    map[string]TranscriptEntry            // latest projected entry per agent for chunk merging
	toolCalls map[string]map[string]TranscriptEntry // agentID -> callId -> latest tool_call entry
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
// without re-fetching the agent's full timeline from Paseo. Consecutive assistant_message and
// reasoning chunks are merged into the active entry at the same seq_start (matching Paseo's
// timeline projection), and tool_call updates with the same callId merge into the matching
// tool_call entry. It returns handled=false when the cursor is missing, the epoch differs,
// there is a seq gap, or the server asks for a resend, so the caller can fall back to Sync.
func (s *TranscriptSync) PushStreamEvent(ctx context.Context, ev paseo.TimelineStreamEvent) (bool, error) {
	if s.cursors == nil || ev.Epoch == "" || ev.Seq <= 0 || len(ev.Item) == 0 || !json.Valid(ev.Item) {
		return false, nil
	}
	cur, have := s.cursors[ev.AgentID]
	if !have || cur.Epoch != ev.Epoch {
		return false, nil
	}
	if ev.Seq != cur.LastSeqEnd+1 && !(ev.Seq == cur.LastSeqStart && cur.LastSeqStart > 0) {
		return false, nil
	}
	if s.active == nil {
		s.active = map[string]TranscriptEntry{}
	}
	if s.toolCalls == nil {
		s.toolCalls = map[string]map[string]TranscriptEntry{}
	}

	var incoming map[string]any
	if err := json.Unmarshal(ev.Item, &incoming); err != nil {
		return false, nil
	}
	inType, _ := incoming["type"].(string)

	var entry TranscriptEntry
	var updateActive = true
	prev, hasPrev := s.active[ev.AgentID]

	switch {
	case inType == "tool_call" && toolCallID(incoming) != "":
		cid := toolCallID(incoming)
		byCall := s.toolCalls[ev.AgentID]
		if existing, ok := byCall[cid]; ok {
			mergedItem, err := mergeToolCallItem(existing.Item, incoming)
			if err != nil {
				return false, nil
			}
			at := existing.At
			if at.IsZero() {
				at = ev.Timestamp
			}
			entry = TranscriptEntry{
				SeqStart: existing.SeqStart,
				SeqEnd:   ev.Seq,
				TurnID:   pickStr(ev.TurnID, existing.TurnID),
				Provider: pickStr(ev.Provider, existing.Provider),
				At:       at,
				Item:     mergedItem,
			}
			byCall[cid] = entry
			updateActive = prev.SeqStart == existing.SeqStart
		} else {
			entry = TranscriptEntry{
				SeqStart: ev.Seq,
				SeqEnd:   ev.Seq,
				TurnID:   ev.TurnID,
				Provider: ev.Provider,
				At:       ev.Timestamp,
				Item:     ev.Item,
			}
			if byCall == nil {
				byCall = map[string]TranscriptEntry{}
				s.toolCalls[ev.AgentID] = byCall
			}
			byCall[cid] = entry
		}

	case hasPrev && ev.Seq == cur.LastSeqEnd+1 && (inType == "assistant_message" || inType == "reasoning") &&
		itemType(prev.Item) == inType && (prev.TurnID == "" || ev.TurnID == "" || prev.TurnID == ev.TurnID):
		mergedItem, err := mergeTextItem(prev.Item, incoming)
		if err != nil {
			return false, nil
		}
		at := prev.At
		if at.IsZero() {
			at = ev.Timestamp
		}
		entry = TranscriptEntry{
			SeqStart: prev.SeqStart,
			SeqEnd:   ev.Seq,
			TurnID:   pickStr(prev.TurnID, ev.TurnID),
			Provider: pickStr(prev.Provider, ev.Provider),
			At:       at,
			Item:     mergedItem,
		}

	default:
		seqStart := ev.Seq
		if ev.Seq == cur.LastSeqStart && cur.LastSeqStart > 0 {
			seqStart = cur.LastSeqStart
		}
		entry = TranscriptEntry{
			SeqStart: seqStart,
			SeqEnd:   ev.Seq,
			TurnID:   ev.TurnID,
			Provider: ev.Provider,
			At:       ev.Timestamp,
			Item:     ev.Item,
		}
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
	if updateActive {
		s.active[ev.AgentID] = entry
		cur.LastSeqStart = entry.SeqStart
	}
	if ev.Seq > cur.LastSeqEnd {
		cur.LastSeqEnd = ev.Seq
	}
	s.cursors[ev.AgentID] = cur
	if s.OnFileEdit != nil && itemMayEditFiles(entry.Item) {
		s.OnFileEdit(ev.AgentID)
	}
	return true, nil
}

func pickStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func itemType(raw json.RawMessage) string {
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	return t.Type
}

func toolCallID(m map[string]any) string {
	for _, k := range []string{"callId", "call_id", "id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func mergeTextItem(prevRaw json.RawMessage, incoming map[string]any) (json.RawMessage, error) {
	var merged map[string]any
	if err := json.Unmarshal(prevRaw, &merged); err != nil {
		return nil, err
	}
	prevText, _ := merged["text"].(string)
	inText, _ := incoming["text"].(string)
	for k, v := range incoming {
		if k == "text" {
			continue
		}
		merged[k] = v
	}
	merged["text"] = prevText + inText
	return json.Marshal(merged)
}

func mergeToolCallItem(prevRaw json.RawMessage, incoming map[string]any) (json.RawMessage, error) {
	var merged map[string]any
	if err := json.Unmarshal(prevRaw, &merged); err != nil {
		return nil, err
	}
	prevDetail, _ := merged["detail"].(map[string]any)
	inDetail, hasInDetail := incoming["detail"].(map[string]any)
	for k, v := range incoming {
		if k == "detail" {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		merged[k] = v
	}
	if hasInDetail {
		if prevDetail == nil {
			prevDetail = map[string]any{}
		}
		for k, v := range inDetail {
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			prevDetail[k] = v
		}
		merged["detail"] = prevDetail
	}
	return json.Marshal(merged)
}

// Reset forgets what the server has, so the next Sync reads its cursors again (a new stream: the
// server may have changed meanwhile).
func (s *TranscriptSync) Reset() { s.cursors, s.synced, s.active, s.toolCalls = nil, nil, nil, nil }

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
		last := tl.Entries[n-1]
		cur.LastSeqStart, cur.LastSeqEnd = last.SeqStart, last.SeqEnd
		if s.active == nil {
			s.active = map[string]TranscriptEntry{}
		}
		s.active[id] = TranscriptEntry{SeqStart: last.SeqStart, SeqEnd: last.SeqEnd, TurnID: last.TurnID, Provider: last.Provider, At: last.Timestamp, Item: last.Item}
	}
	if whole && s.toolCalls != nil {
		delete(s.toolCalls, id)
	}
	for _, e := range tl.Entries {
		var m map[string]any
		if json.Unmarshal(e.Item, &m) == nil && m["type"] == "tool_call" {
			if cid := toolCallID(m); cid != "" {
				if s.toolCalls == nil {
					s.toolCalls = map[string]map[string]TranscriptEntry{}
				}
				if s.toolCalls[id] == nil {
					s.toolCalls[id] = map[string]TranscriptEntry{}
				}
				s.toolCalls[id][cid] = TranscriptEntry{SeqStart: e.SeqStart, SeqEnd: e.SeqEnd, TurnID: e.TurnID, Provider: e.Provider, At: e.Timestamp, Item: e.Item}
			}
		}
	}
	s.cursors[id] = cur
	return nil
}
