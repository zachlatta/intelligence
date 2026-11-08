package devbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zachlatta/intelligence/internal/host"
	"github.com/zachlatta/intelligence/internal/usage"
)

// streamServer is the server's side of the stream, scripted by the test.
type streamServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	auth   []string
	got    []Frame
	conns  []*websocket.Conn
	refuse bool                // answer 401 instead of upgrading
	stall  bool                // after the hello, stop reading (and so answering pings), as a hung server does
	async  bool                // answer each frame on its own goroutine, so a slow answer holds up no other
	answer func(f Frame) Frame // the ack for a box frame; nil never acks
}

func newStreamServer(t *testing.T) *streamServer {
	s := &streamServer{t: t, answer: func(f Frame) Frame { return Frame{Type: AckFrame, ID: f.ID} }}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, "srv-v2")
		s.mu.Lock()
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		refuse := s.refuse
		s.mu.Unlock()
		if r.URL.Path != StreamPath("box") {
			http.NotFound(w, r)
			return
		}
		if refuse {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		ctx := r.Context()
		WriteFrame(ctx, c, Frame{Type: HelloFrame, Hello: &Hello{
			Cursors: map[string]TranscriptCursor{"a1": {Epoch: "e1", LastSeqStart: 4, LastSeqEnd: 5}}}})
		s.mu.Lock()
		stall := s.stall
		s.mu.Unlock()
		if stall {
			<-ctx.Done()
			return
		}
		for {
			f, err := ReadFrame(ctx, c)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.got = append(s.got, f)
			answer, async := s.answer, s.async
			s.mu.Unlock()
			switch {
			case answer != nil && async:
				go func() { WriteFrame(ctx, c, answer(f)) }()
			case answer != nil:
				WriteFrame(ctx, c, answer(f))
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *streamServer) push(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		WriteFrame(context.Background(), c, f)
	}
}

func link(t *testing.T, s *streamServer) (*Link, *[]string) {
	var mu sync.Mutex
	versions := &[]string{}
	l := &Link{Server: s.srv.URL, Token: "tok", Box: "box", RetryEvery: 20 * time.Millisecond, AckTimeout: 300 * time.Millisecond,
		ServerVersion: func(v string) { mu.Lock(); *versions = append(*versions, v); mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l, versions
}

func connected(t *testing.T, l *Link) {
	t.Helper()
	select {
	case <-l.Connected():
	case <-time.After(5 * time.Second):
		t.Fatalf("never connected: %s", l.Err())
	}
}

func TestLinkDialsWithTheTokenAndReadsTheHello(t *testing.T) {
	s := newStreamServer(t)
	l, versions := link(t, s)
	connected(t, l)
	cur, err := l.TranscriptCursors(context.Background())
	if err != nil || cur["a1"].LastSeqStart != 4 {
		t.Fatalf("cursors = %v, %v", cur, err)
	}
	if s.auth[0] != "Bearer tok" || (*versions)[0] != "srv-v2" {
		t.Errorf("auth %q, versions %v", s.auth[0], *versions)
	}
	if err := l.Report(context.Background(), Report{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if f := s.got[0]; f.Type != ReportFrame || f.Report.Name != "box" || f.ID == 0 {
		t.Errorf("frame = %+v", f)
	}
}

func TestLinkPassesTheServerVersionOnARefusedDial(t *testing.T) {
	s := newStreamServer(t)
	s.refuse = true
	l, versions := link(t, s)
	deadline := time.Now().Add(5 * time.Second)
	for l.Err() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(l.Err(), "401") {
		t.Errorf("err = %q", l.Err())
	}
	if len(*versions) == 0 || (*versions)[0] != "srv-v2" {
		t.Errorf("versions = %v: a box must update to a server that refuses it", *versions)
	}
	if err := l.Report(context.Background(), Report{}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("report while refused: %v", err)
	}
}

func TestLinkMapsAckCodes(t *testing.T) {
	s := newStreamServer(t)
	s.answer = func(f Frame) Frame {
		switch f.Transcript.AgentID {
		case "resend":
			return Frame{Type: AckFrame, ID: f.ID, Code: AckResend}
		case "later":
			return Frame{Type: AckFrame, ID: f.ID, Code: AckNotYet}
		}
		return Frame{Type: AckFrame, ID: f.ID, Error: "too big"}
	}
	l, _ := link(t, s)
	connected(t, l)
	ctx := context.Background()
	if err := l.Transcript(ctx, TranscriptBatch{AgentID: "resend"}); !errors.Is(err, ErrResend) {
		t.Errorf("resend: %v", err)
	}
	if err := l.Transcript(ctx, TranscriptBatch{AgentID: "later"}); !errors.Is(err, ErrNotYet) {
		t.Errorf("not yet: %v", err)
	}
	if err := l.Transcript(ctx, TranscriptBatch{AgentID: "x"}); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Errorf("refused: %v", err)
	}
}

func TestLinkDeliversCommands(t *testing.T) {
	s := newStreamServer(t)
	l, _ := link(t, s)
	connected(t, l)
	s.push(Frame{Type: CommandFrame, Command: &Command{ID: "c1", Kind: ArchiveAgentKind}})
	select {
	case c := <-l.Commands():
		if c.ID != "c1" || c.Kind != ArchiveAgentKind {
			t.Errorf("command = %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no command")
	}
}

func TestLinkRedialsWhenTheServerStopsAnswering(t *testing.T) {
	s := newStreamServer(t)
	s.answer = nil
	l, _ := link(t, s)
	connected(t, l)
	if err := l.Report(context.Background(), Report{}); err == nil || !strings.Contains(err.Error(), "acknowledge") {
		t.Fatalf("err = %v", err)
	}
	connected(t, l) // dropped and dialed again
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	if n < 2 {
		t.Errorf("%d connections, want a fresh one after the silent server", n)
	}
}

// frames are the box frames of one type the server has received.
func (s *streamServer) frames(typ string) []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Frame
	for _, f := range s.got {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func (s *streamServer) connections() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.conns) }

// TestLinkPingsAHungServer: a box with nothing to send still notices a server that stopped
// answering, and dials again.
func TestLinkPingsAHungServer(t *testing.T) {
	s := newStreamServer(t)
	s.stall = true
	l := &Link{Server: s.srv.URL, Token: "tok", Box: "box", RetryEvery: 20 * time.Millisecond, PingEvery: 50 * time.Millisecond,
		AckTimeout: 200 * time.Millisecond}
	go l.Run(t.Context())
	connected(t, l)
	deadline := time.Now().Add(5 * time.Second)
	for s.connections() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("never dialed again")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTranscriptSyncRereadsCursorsAfterReset(t *testing.T) {
	r := newSyncRig(t, "a1")
	calls := 0
	r.ts.Cursors = func(ctx context.Context) (map[string]TranscriptCursor, error) { calls++; return r.srv.cursors(ctx) }
	r.sync(t)
	r.sync(t)
	r.ts.Reset()
	r.sync(t)
	if calls != 2 {
		t.Errorf("read cursors %d times, want once per Reset", calls)
	}
}

func TestLinkSendsUsage(t *testing.T) {
	s := newStreamServer(t)
	l, _ := link(t, s)
	connected(t, l)
	rep := usage.Report{CollectedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Sources: []usage.Source{{Provider: usage.Muse, Accounts: []usage.Account{{ID: "default", Windows: []usage.Window{{ID: "7d", UsedPct: 25}}}}}}}
	if err := l.Usage(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 || s.got[0].Type != UsageFrame || s.got[0].Usage == nil || s.got[0].Usage.Sources[0].Accounts[0].Windows[0].UsedPct != 25 {
		t.Fatalf("got %+v", s.got)
	}
}

func TestLinkSendsHostMinutes(t *testing.T) {
	s := newStreamServer(t)
	l, _ := link(t, s)
	connected(t, l)
	if err := l.Host(context.Background(), host.Report{Minutes: []host.Minute{{Samples: 12}}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 || s.got[0].Type != HostFrame || s.got[0].Host == nil || s.got[0].Host.Minutes[0].Samples != 12 {
		t.Fatalf("got %+v", s.got)
	}
}
