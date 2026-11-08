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
	b := &Client{Name: "host", Version: "v1", StatusEvery: 20 * time.Millisecond, LiveEvery: 20 * time.Millisecond,
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
