package paseo

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Cursor is a position in an agent's timeline. Seqs are only comparable within one epoch: the
// daemon starts a new epoch when it replaces a timeline (a rewind, a reloaded session).
type Cursor struct {
	Epoch string `json:"epoch"`
	Seq   int    `json:"seq"`
}

// Timeline is part or all of an agent's projected timeline.
type Timeline struct {
	Epoch   string
	Reset   bool // the cursor was from another epoch or stale: Entries is the whole timeline
	Entries []TimelineEntry
}

// TimelineEntry is one projected entry: a message, reasoning, a tool call and so on. One entry can
// cover several seqs (streamed chunks merged). Item is the daemon's item, whole.
type TimelineEntry struct {
	SeqStart  int             `json:"seqStart"`
	SeqEnd    int             `json:"seqEnd"`
	TurnID    string          `json:"turnId"`
	Provider  string          `json:"provider"`
	Timestamp time.Time       `json:"timestamp"`
	Item      json.RawMessage `json:"item"`
}

// Timeline reads agentID's projected timeline: all of it with after nil, else the entries that start
// after after.Seq. A cursor the daemon cannot continue from reads the whole timeline, with Reset set.
//
// The daemon loads an agent to answer this, resuming a closed one: read only agents that are
// already loaded (running, idle, error), or the read starts a closed agent's process.
func (c *Client) Timeline(ctx context.Context, agentID string, after *Cursor) (Timeline, error) {
	req := map[string]any{"type": "fetch_agent_timeline_request", "agentId": agentID, "direction": "tail", "limit": 0}
	if after != nil {
		req["direction"], req["cursor"] = "after", after
	}
	var p struct {
		Epoch       string          `json:"epoch"`
		Reset       bool            `json:"reset"`
		StaleCursor bool            `json:"staleCursor"`
		Gap         bool            `json:"gap"`
		Entries     []TimelineEntry `json:"entries"`
		Error       *string         `json:"error"`
	}
	if err := c.request(ctx, req, &p); err != nil {
		return Timeline{}, err
	}
	if p.Error != nil {
		return Timeline{}, errors.New("fetch_agent_timeline: " + *p.Error)
	}
	if after != nil && (p.Reset || p.StaleCursor || p.Gap || p.Epoch != after.Epoch) {
		whole, err := c.Timeline(ctx, agentID, nil)
		whole.Reset = true
		return whole, err
	}
	return Timeline{Epoch: p.Epoch, Entries: p.Entries}, nil
}
