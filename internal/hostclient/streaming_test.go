package hostclient

import (
	"testing"
	"time"

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
