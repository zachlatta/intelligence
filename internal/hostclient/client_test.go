package hostclient

import (
	"context"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
	"github.com/zachlatta/intelligence/internal/usage"
)

// hostRig runs a Client against a fake daemon and a scripted server, with short intervals.
func hostRig(t *testing.T, s *streamServer, d *paseotest.Daemon, opts ...func(*Client)) *Client {
	t.Helper()
	b := &Client{Name: "host", Version: "v1", StatusEvery: 20 * time.Millisecond, LiveEvery: 20 * time.Millisecond, DiffEvery: 20 * time.Millisecond,
		Settle: 10 * time.Millisecond, Logf: t.Logf,
		Watcher: &Watcher{RetryEvery: 20 * time.Millisecond, Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: d.URL(), Token: tok, ClientID: "test"})
		}},
		Link: &Link{Server: s.srv.URL, Token: "tok", Host: "host", RetryEvery: 20 * time.Millisecond, AckTimeout: 5 * time.Second}}
	for _, o := range opts {
		o(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return b
}

// reports waits until the server has n reports, and returns them.
func reports(t *testing.T, s *streamServer, n int, what string) []Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.frames(ReportFrame)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d reports, want %d", what, len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostReportsOnConnectAndOnChangeOnly(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	hostRig(t, s, d)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.frames(ReportFrame)
		if n := len(got); n > 0 && len(got[n-1].Report.Paseo.Agents) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reported the daemon's agents: %d reports", len(got))
		}
		time.Sleep(10 * time.Millisecond)
	}
	settled := len(s.frames(ReportFrame))
	time.Sleep(300 * time.Millisecond) // 15 status reads, nothing changed
	if n := len(s.frames(ReportFrame)); n != settled {
		t.Fatalf("%d reports with nothing changed, want %d", n, settled)
	}
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a3", "w2", "idle")})
	got := reports(t, s, settled+1, "after a change")
	if r := got[len(got)-1].Report; r.Name != "host" || r.Version != "v1" || len(r.Paseo.Agents) != 3 {
		t.Errorf("report = %+v", r)
	}
}

func TestHostReportsAgainOnANewStream(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	hostRig(t, s, d)
	n := len(reports(t, s, 1, "first"))
	s.mu.Lock()
	for _, c := range s.conns {
		c.CloseNow()
	}
	s.mu.Unlock()
	reports(t, s, n+1, "on the new stream")
}

// TestHostReportsWhileATranscriptUploads: a transcript upload the server is slow to store does
// not hold up the host's reports.
func TestHostReportsWhileATranscriptUploads(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	d.SetTimeline("a1", "e9", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.async = true
	s.answer = func(f Frame) Frame {
		if f.Type == TranscriptFrame {
			<-release
		}
		return Frame{Type: AckFrame, ID: f.ID}
	}
	hostRig(t, s, d)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.frames(TranscriptFrame)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never uploaded a transcript")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := len(s.frames(ReportFrame))
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a3", "w2", "idle")})
	reports(t, s, n+1, "while a transcript upload waits")
}

func TestHostSendsItsPlansUsage(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	hostRig(t, s, d, func(b *Client) {
		b.Usage = &UsageSync{RetryEvery: 20 * time.Millisecond,
			Read: func(context.Context) usage.Report { return usage.Report{CollectedAt: time.Unix(1, 0)} }}
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(s.frames(UsageFrame)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never sent usage")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostSendsItsWorkspacesDiffs(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	d.SetDiff("/w/w1", paseotest.DiffFile("main.go", 2, 1))
	hostRig(t, s, d)
	deadline := time.Now().Add(5 * time.Second)
	for {
		byID := map[string]*WorkspaceDiff{}
		for _, f := range s.frames(DiffFrame) {
			byID[f.Diff.WorkspaceID] = f.Diff
		}
		w1, w2 := byID["w1"], byID["w2"]
		if w1 != nil && w2 != nil {
			if len(w1.Files) != 1 || w1.Files[0].Path != "main.go" || w1.Additions != 2 || w1.Directory != "/w/w1" ||
				w2.Error != "Not a git repository: /w/w2" {
				t.Fatalf("w1 = %+v, w2 = %+v", w1, w2)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("diffs sent: %v", byID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a1 works in w1: its diff is read again and a change goes up
	d.SetDiff("/w/w1", paseotest.DiffFile("main.go", 3, 1))
	for {
		got := s.frames(DiffFrame)
		if last := got[len(got)-1].Diff; last.WorkspaceID == "w1" && last.Additions == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a running agent's change never went up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostWatchdogHandlesExistingAndPushedPrompts(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	a := paseotest.Agent("a1", "w1", "running")
	a["currentModeId"] = "bypassPermissions"
	a["pendingPermissions"] = []any{map[string]any{"id": "permission-first", "name": "Bash"}}
	d.SetAgents(a)
	hostRig(t, s, d)
	waitAudit := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, f := range s.frames(PermissionFrame) {
				if f.Permission.PermissionID == id && f.Permission.Status == "denied" {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no denied audit for", id)
	}
	waitAudit("permission-first")
	a = paseotest.Agent("a1", "w1", "running")
	a["currentModeId"] = "bypassPermissions"
	a["pendingPermissions"] = []any{map[string]any{"id": "permission-next", "name": "Bash"}}
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": a})
	waitAudit("permission-next")
	if n := len(d.Requests("agent_permission_response")); n != 2 {
		t.Fatal("denials", n)
	}
}

func TestHostRestoresPermissionAuditsOnStartAndBothReconnects(t *testing.T) {
	server, d := newStreamServer(t), daemon(t)
	old := time.Now().Add(-time.Hour)
	server.answer = func(f Frame) Frame {
		ack := Frame{Type: AckFrame, ID: f.ID}
		if f.Type == PermissionRestoreFrame {
			ack.PermissionRestore = &PermissionRestore{Records: []PermissionResolution{
				{AgentID: "gone", PermissionID: "p", Status: "stuck", At: old, FirstSeenAt: old, StuckAt: old, DeniedAt: old},
			}}
		}
		return ack
	}
	hostRig(t, server, d)
	waitCleared := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			count := 0
			for _, f := range server.frames(PermissionFrame) {
				if f.Permission.Status == "cleared" {
					count++
				}
			}
			if count >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("did not reconcile restored audit", n)
	}
	waitCleared(1)
	server.mu.Lock()
	for _, c := range server.conns {
		c.CloseNow()
	}
	server.mu.Unlock()
	waitCleared(2)
	d.DropAll()
	waitCleared(3)
}

func TestHostSendsPeriodicSnapshotsWithoutChanges(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	ticks := make(chan time.Time)
	seen := make(chan Frame, 16)
	s.answer = func(f Frame) Frame {
		if f.Type == ReportFrame {
			seen <- f
		}
		return Frame{Type: AckFrame, ID: f.ID}
	}
	hostRig(t, s, d, func(c *Client) {
		c.reportTicks = ticks
		c.StatusEvery = time.Hour
		c.Settle = -1 // no debounce delay in this test
	})
	// Initial daemon discovery is asynchronous; report delivery is our barrier.
	for f := range seen {
		if f.Report.Paseo.Connected && len(f.Report.Paseo.Agents) == 2 {
			break
		}
	}
	for range 2 {
		ticks <- time.Time{}
		f := <-seen
		if f.Report.Name != "host" || len(f.Report.Paseo.Agents) != 2 {
			t.Fatalf("periodic report lost unchanged snapshot: %+v", f.Report)
		}
	}
}
