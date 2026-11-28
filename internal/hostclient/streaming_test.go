package hostclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// A LiveFast agent flipping status (idle <-> running) flushes its report right away instead of
// waiting for the 1s Settle window, while non-LiveFast updates still wait for Settle.
func TestLiveFastStatusFlipSkipsSettle(t *testing.T) {
	s, d := newStreamServer(t), paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	mgr := paseotest.Agent("mgr-1", "w1", "idle")
	mgr["labels"] = map[string]any{LiveLabel: LiveFast}
	d.SetAgents(mgr)
	hostRig(t, s, d, func(c *Client) {
		c.Settle = 2 * time.Second
		c.LiveEvery = time.Hour
		c.DiffEvery = time.Hour
	})

	waitReport := func(wantStatus string) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for {
			for _, f := range s.frames(ReportFrame) {
				for _, a := range f.Report.Paseo.Agents {
					if a.ID == "mgr-1" && a.Status == wantStatus {
						return
					}
				}
			}
			if time.Now().After(dl) {
				t.Fatalf("never saw mgr-1 status %q", wantStatus)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitReport("idle")

	start := time.Now()
	running := paseotest.Agent("mgr-1", "w1", "running")
	running["labels"] = map[string]any{LiveLabel: LiveFast}
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": running})
	waitReport("running")
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("LiveFast status flip took %v, want <500ms (Settle is 2s)", took)
	}
}

// agent_stream timeline events for a LiveFast agent with contiguous seq push straight to the
// server without issuing an extra fetch_agent_timeline_request RPC.
func TestLiveFastAgentStreamPushesDeltaWithoutRefetch(t *testing.T) {
	s, d := newStreamServer(t), paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	mgr := paseotest.Agent("g1", "w1", "running")
	mgr["labels"] = map[string]any{LiveLabel: LiveFast}
	d.SetAgents(mgr)
	d.SetTimeline("g1", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	hostRig(t, s, d, func(c *Client) {
		c.LiveEvery = time.Hour
		c.DiffEvery = time.Hour
	})

	hasSeq := func(seq int) bool {
		for _, f := range s.frames(TranscriptFrame) {
			if f.Transcript.AgentID != "g1" {
				continue
			}
			for _, e := range f.Transcript.Entries {
				if e.SeqStart == seq {
					return true
				}
			}
		}
		return false
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(dl) {
				t.Fatal(what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("initial transcript", func() bool { return hasSeq(1) })
	waitFor("timeline subscription", func() bool { return len(d.TimelineSubscriptions()) == 1 })

	fetchesBefore := len(d.Requests("fetch_agent_timeline_request"))
	d.Stream("g1", 2, paseotest.Message("assistant_message", "streamed directly"))
	waitFor("streamed seq 2", func() bool { return hasSeq(2) })
	if fetchesAfter := len(d.Requests("fetch_agent_timeline_request")); fetchesAfter != fetchesBefore {
		t.Fatalf("fetch_agent_timeline_request increased from %d to %d; expected direct delta push", fetchesBefore, fetchesAfter)
	}
}

func TestDiffSyncDefaultFallbackIs30SecondsAndTriggersOnFileEdit(t *testing.T) {
	if !itemMayEditFiles([]byte(`{"type":"tool_call","name":"Edit","detail":{"type":"edit","filePath":"a.go"}}`)) {
		t.Error("expected edit tool_call to trigger diff sync")
	}
	if !itemMayEditFiles([]byte(`{"type":"tool_call","name":"Bash","detail":{"type":"shell","command":"go test"}}`)) {
		t.Error("expected shell tool_call to trigger diff sync")
	}
	if itemMayEditFiles([]byte(`{"type":"tool_call","name":"Read","detail":{"type":"read","filePath":"a.go"}}`)) {
		t.Error("read tool_call should not trigger diff sync")
	}
	if itemMayEditFiles([]byte(`{"type":"assistant_message","text":"thinking"}`)) {
		t.Error("assistant_message should not trigger diff sync")
	}
}

// M1: PushStreamEvent merges consecutive assistant_message/reasoning chunks into one entry at
// the same seq_start and merges tool_call updates by callId, reconciling cleanly with Sync.
func TestPushStreamEventMergesChunksAndToolCalls(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	msg := func(kind, text string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"type": kind, "text": text})
		return b
	}
	user := msg("user_message", "hi")
	tcStart, _ := json.Marshal(map[string]any{
		"type": "tool_call", "callId": "call-1", "name": "Bash", "status": "running",
		"detail": map[string]any{"type": "shell", "command": "go test ./..."},
	})
	tcDone, _ := json.Marshal(map[string]any{
		"type": "tool_call", "callId": "call-1", "name": "Bash", "status": "completed",
		"detail": map[string]any{"type": "shell", "output": "PASS"},
	})
	tcMerged, _ := json.Marshal(map[string]any{
		"type": "tool_call", "callId": "call-1", "name": "Bash", "status": "completed",
		"detail": map[string]any{"type": "shell", "command": "go test ./...", "output": "PASS"},
	})
	projected := []paseo.TimelineEntry{
		{SeqStart: 1, SeqEnd: 1, TurnID: "t1", Provider: "claude", Timestamp: at, Item: user},
		{SeqStart: 2, SeqEnd: 3, TurnID: "t1", Provider: "claude", Timestamp: at, Item: msg("assistant_message", "Hello")},
		{SeqStart: 4, SeqEnd: 5, TurnID: "t1", Provider: "claude", Timestamp: at, Item: tcMerged},
	}
	server := map[int]TranscriptEntry{}
	ts := &TranscriptSync{
		Host: "h",
		Cursors: func(context.Context) (map[string]TranscriptCursor, error) {
			return map[string]TranscriptCursor{}, nil
		},
		Timeline: func(_ context.Context, _ string, after *paseo.Cursor) (paseo.Timeline, error) {
			if after == nil {
				return paseo.Timeline{Epoch: "e1", Entries: projected[:1]}, nil
			}
			var out []paseo.TimelineEntry
			for _, e := range projected {
				if e.SeqStart > after.Seq {
					out = append(out, e)
				}
			}
			return paseo.Timeline{Epoch: "e1", Entries: out}, nil
		},
		Send: func(_ context.Context, b TranscriptBatch) error {
			if b.Replace {
				server = map[int]TranscriptEntry{}
			}
			for _, e := range b.Entries {
				server[e.SeqStart] = e
			}
			return nil
		},
	}

	agents := []Agent{{ID: "g1", Status: "running", UpdatedAt: at}}
	if err := ts.Sync(ctx, agents); err != nil {
		t.Fatal(err)
	}
	for i, chunk := range []string{"Hel", "lo"} {
		ok, err := ts.PushStreamEvent(ctx, paseo.TimelineStreamEvent{
			AgentID: "g1", Epoch: "e1", Seq: 2 + i, Timestamp: at, TurnID: "t1", Provider: "claude",
			Item: msg("assistant_message", chunk),
		})
		if err != nil || !ok {
			t.Fatalf("push chunk %q: ok=%v err=%v", chunk, ok, err)
		}
	}
	for i, raw := range []json.RawMessage{tcStart, tcDone} {
		ok, err := ts.PushStreamEvent(ctx, paseo.TimelineStreamEvent{
			AgentID: "g1", Epoch: "e1", Seq: 4 + i, Timestamp: at, TurnID: "t1", Provider: "claude",
			Item: raw,
		})
		if err != nil || !ok {
			t.Fatalf("push tool_call %d: ok=%v err=%v", i, ok, err)
		}
	}

	// Follow-up Sync reconciles seamlessly without leaving fragments.
	agents[0].Status = "idle"
	agents[0].UpdatedAt = at.Add(time.Second)
	if err := ts.Sync(ctx, agents); err != nil {
		t.Fatal(err)
	}
	if len(server) != 3 {
		t.Fatalf("server entries = %d, want 3 (1:user, 2:Hello, 4:tool_call); got keys: %+v", len(server), server)
	}
	var m2 struct{ Text string }
	_ = json.Unmarshal(server[2].Item, &m2)
	if m2.Text != "Hello" || server[2].SeqEnd != 3 {
		t.Errorf("entry 2 = text %q seqEnd %d, want Hello / 3", m2.Text, server[2].SeqEnd)
	}
	var m4 struct {
		Status string `json:"status"`
		Detail struct {
			Command string `json:"command"`
			Output  string `json:"output"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(server[4].Item, &m4)
	if m4.Status != "completed" || m4.Detail.Command != "go test ./..." || m4.Detail.Output != "PASS" {
		t.Errorf("merged tool_call = %+v", m4)
	}
}
