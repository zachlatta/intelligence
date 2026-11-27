package hostclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// The host subscribes to the daemon's pushes of a live agent's timeline (LiveLabel) and copies
// its transcript when one arrives, not on a timer: with nothing pushed nothing is read, and an
// agent not labelled live waits for LiveEvery.
func TestHostCopiesALiveAgentsTranscriptWhenTheDaemonPushesIt(t *testing.T) {
	s, d := newStreamServer(t), paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	live := paseotest.Agent("g1", "w1", "running")
	live["labels"] = map[string]any{LiveLabel: LiveFast}
	d.SetAgents(live, paseotest.Agent("g2", "w1", "running"))
	d.SetTimeline("g1", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	d.SetTimeline("g2", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	hostRig(t, s, d, func(c *Client) { c.LiveEvery = time.Hour })
	upload := func(agent string, seq int) bool {
		for _, f := range s.frames(TranscriptFrame) {
			for _, e := range f.Transcript.Entries {
				if f.Transcript.AgentID == agent && e.SeqStart == seq {
					return true
				}
			}
		}
		return false
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	eventually("never uploaded the first transcripts", func() bool { return upload("g1", 1) && upload("g2", 1) })
	eventually("never subscribed to the live agent's timeline", func() bool {
		return strings.Join(d.TimelineSubscriptions(), "|") == "g1"
	})
	reads := len(d.Requests("fetch_agent_timeline_request"))
	d.SetTimeline("g1", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "on it")))
	d.SetTimeline("g2", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "on it")))
	time.Sleep(500 * time.Millisecond)
	if n := len(d.Requests("fetch_agent_timeline_request")); n != reads || upload("g1", 2) {
		t.Fatalf("read timelines with nothing pushed: %d reads, want %d", n, reads)
	}
	start := time.Now()
	d.Stream("g1", 2, paseotest.Message("assistant_message", "on it"))
	eventually("the pushed entry was not copied", func() bool { return upload("g1", 2) })
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("copied %v after the push", took)
	}
	if upload("g2", 2) {
		t.Error("an agent not labelled live was copied before LiveEvery")
	}
}

// However many live agents push at once, the steady ones are copied at most SteadyBudget times a
// second together and each at most every SteadyGap, and take hands out a LiveFast one regardless
// (that the host's loop takes it at once is TestAFastAgentsPushIsNotHeldBehindAWaitingSteadyOne).
func TestLivePaceBoundsTheHostsWork(t *testing.T) {
	p := &livePace{fast: 100 * time.Millisecond, steady: time.Second, budget: 8, last: map[string]time.Time{}, dirty: map[string]bool{}}
	var agents []Agent
	for i := range 40 {
		id := fmt.Sprintf("w%d", i)
		agents = append(agents, Agent{ID: id, Live: "steady"})
		p.dirty[id] = true
	}
	agents = append(agents, Agent{ID: "mgr", Live: LiveFast})
	p.dirty["mgr"] = true
	t0 := time.Unix(1000, 0)
	ready, wait := p.take(agents, t0)
	steady := 0
	fast := false
	for _, a := range ready {
		if a.ID == "mgr" {
			fast = true
		} else {
			steady++
		}
	}
	if !fast || steady != 8 || wait <= 0 || wait > time.Second {
		t.Fatalf("first take: fast %v, %d steady, wait %v", fast, steady, wait)
	}
	// the manager again 100ms later, but no steady one until the next second
	p.dirty["mgr"] = true
	ready, _ = p.take(agents, t0.Add(150*time.Millisecond))
	if len(ready) != 1 || ready[0].ID != "mgr" {
		t.Fatalf("second take = %+v", ready)
	}
	total := 8
	for s := 1; s <= 5; s++ {
		ready, _ = p.take(agents, t0.Add(time.Duration(s)*time.Second+time.Millisecond))
		if len(ready) > 8 {
			t.Fatalf("second %d: %d copies, want at most 8", s, len(ready))
		}
		total += len(ready)
	}
	if total != 40 {
		t.Errorf("copied %d of 40 pushed agents in 6 seconds", total)
	}
}

// watch_agent sets an agent's LiveLabel on the daemon (merged into its other labels), which the
// host then follows: fast while a page watches it, steady after.
func TestWatchAgentSetsTheLiveLabel(t *testing.T) {
	d := paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	a := paseotest.Agent("g1", "w1", "running")
	a["labels"] = map[string]any{LiveLabel: LiveSteady, "intelligence.grimm.name": "athena"}
	d.SetAgents(a)
	w := watcher(t, d)
	eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 1 })
	if _, err := execute(t, w, WatchAgentKind, `{"agentId":"g1","live":"fast"}`); err != nil {
		t.Fatal(err)
	}
	eventually(t, w, "the label", func(s PaseoStatus) bool { return len(s.Agents) == 1 && s.Agents[0].Live == LiveFast })
	for _, bad := range []string{`{"agentId":"g1"}`, `{"agentId":"g1","live":"always"}`} {
		if _, err := ParseCommand(WatchAgentKind, []byte(bad)); err == nil {
			t.Errorf("watch_agent %s parsed", bad)
		}
	}
	if _, err := ParseCommand(CancelAgentKind, []byte(`{"agentId":"g1","live":"fast"}`)); err == nil {
		t.Error("cancel_agent took live")
	}
}

// A steady worker waiting for its gap does not hold back the manager's (LiveFast) copy: the
// manager's push is copied at once, through the host's own loop.
func TestAFastAgentsPushIsNotHeldBehindAWaitingSteadyOne(t *testing.T) {
	s, d := newStreamServer(t), paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	mgr := paseotest.Agent("g1", "w1", "running")
	mgr["labels"] = map[string]any{LiveLabel: LiveFast}
	wk := paseotest.Agent("g2", "w1", "running")
	wk["labels"] = map[string]any{LiveLabel: LiveSteady}
	d.SetAgents(mgr, wk)
	msg := func(n int) []map[string]any {
		var out []map[string]any
		for i := 1; i <= n; i++ {
			out = append(out, paseotest.Entry(i, i, paseotest.Message("assistant_message", "x")))
		}
		return out
	}
	d.SetTimeline("g1", "e1", msg(1)...)
	d.SetTimeline("g2", "e1", msg(1)...)
	hostRig(t, s, d, func(c *Client) { c.LiveEvery = time.Hour })
	upload := func(agent string, seq int) bool {
		for _, f := range s.frames(TranscriptFrame) {
			for _, e := range f.Transcript.Entries {
				if f.Transcript.AgentID == agent && e.SeqStart == seq {
					return true
				}
			}
		}
		return false
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(dl) {
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("first", func() bool { return upload("g1", 1) && upload("g2", 1) })
	wait("sub", func() bool { return len(d.TimelineSubscriptions()) == 1 })
	// worker pushes: copied now (sets its last), then pushes again: held for ~1s
	d.SetTimeline("g2", "e1", msg(2)...)
	d.Stream("g2", 2, paseotest.Message("assistant_message", "x"))
	wait("worker 2", func() bool { return upload("g2", 2) })
	workerCopied := time.Now()
	d.SetTimeline("g2", "e1", msg(3)...)
	d.Stream("g2", 3, paseotest.Message("assistant_message", "x"))
	time.Sleep(50 * time.Millisecond)
	// now the manager pushes
	d.SetTimeline("g1", "e1", msg(2)...)
	start := time.Now()
	d.Stream("g1", 2, paseotest.Message("assistant_message", "x"))
	wait("manager 2", func() bool { return upload("g1", 2) })
	took := time.Since(start)
	t.Logf("manager copied %v after its push, while a steady worker was waiting", took)
	if took > 300*time.Millisecond {
		t.Errorf("manager held back %v by a waiting steady worker", took)
	}
	// the worker's second push still waits for its gap, and is copied then, not dropped
	if upload("g2", 3) {
		t.Error("the steady worker was copied again before its gap")
	}
	wait("worker 3", func() bool { return upload("g2", 3) })
	if gap := time.Since(workerCopied); gap < 900*time.Millisecond {
		t.Errorf("the steady worker was copied again %v after its last copy, want about 1s", gap)
	}
}
