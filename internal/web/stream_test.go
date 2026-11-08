package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/usage"
)

// rawHost is a host's end of the stream, driven frame by frame.
type rawHost struct {
	t     *testing.T
	conn  *websocket.Conn
	hello hostclient.Hello
	next  uint64
}

func wsURL(srv *httptest.Server, host string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + hostclient.StreamPath(host)
}

// dialStream opens host's stream to h with token; the response is the refusal when err is set.
func dialStream(t *testing.T, srv *httptest.Server, host, token string) (*rawHost, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, host), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, resp, err
	}
	conn.SetReadLimit(hostclient.MaxFrame)
	t.Cleanup(func() { conn.CloseNow() })
	f, err := hostclient.ReadFrame(ctx, conn)
	if err != nil || f.Type != hostclient.HelloFrame {
		t.Fatalf("hello = %+v, %v", f, err)
	}
	return &rawHost{t: t, conn: conn, hello: *f.Hello}, resp, nil
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func openStream(t *testing.T, h http.Handler, host string) *rawHost {
	t.Helper()
	b, _, err := dialStream(t, serve(t, h), host, keyOf(t, h, host))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// call sends f and returns its ack, skipping any command frames.
func (b *rawHost) call(f hostclient.Frame) hostclient.Frame {
	b.t.Helper()
	b.next++
	f.ID = b.next
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hostclient.WriteFrame(ctx, b.conn, f); err != nil {
		b.t.Fatal(err)
	}
	for {
		got, err := hostclient.ReadFrame(ctx, b.conn)
		if err != nil {
			b.t.Fatalf("waiting for the ack of %s: %v", f.Type, err)
		}
		if got.Type == hostclient.AckFrame {
			if got.ID != f.ID {
				b.t.Fatalf("ack %d, want %d", got.ID, f.ID)
			}
			return got
		}
	}
}

// command reads frames until a command arrives.
func (b *rawHost) command() hostclient.Command {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		f, err := hostclient.ReadFrame(ctx, b.conn)
		if err != nil {
			b.t.Fatalf("waiting for a command: %v", err)
		}
		if f.Type == hostclient.CommandFrame {
			return *f.Command
		}
	}
}

// sendReport sends rep on its host's stream, which stays open (so the host is online), and fails
// the test unless it is stored.
func sendReport(t *testing.T, h http.Handler, rep hostclient.Report) {
	t.Helper()
	b := openStream(t, h, rep.Name)
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatalf("report refused: %s", ack.Error)
	}
}

// sendBatch sends a transcript batch on its host's stream and returns the ack.
func sendBatch(t *testing.T, h http.Handler, b hostclient.TranscriptBatch) hostclient.Frame {
	t.Helper()
	s := openStream(t, h, b.Host)
	defer s.conn.Close(websocket.StatusNormalClosure, "")
	return s.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &b})
}

func TestStreamNeedsTheHostsOwnKey(t *testing.T) {
	h, d := dbServer(t)
	srv := serve(t, h)
	for _, key := range []string{"", "wrong", keyOf(t, h, "host-2")} { // another host's key is no good
		_, resp, err := dialStream(t, srv, "host-1", key)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: %v, want 401", key, err)
		}
		if resp != nil && resp.Header.Get(hostclient.VersionHeader) != "v" {
			t.Errorf("a refused stream must still name the server's version, for the host to update")
		}
	}
	if len(hosts(t, d)) != 0 {
		t.Fatalf("an unauthorized host was recorded")
	}
}

func TestStreamRefusesABadName(t *testing.T) {
	h, _ := dbServer(t)
	if _, resp, err := dialStream(t, serve(t, h), "Bad_Name", keyOf(t, h, "host-1")); err == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%v, want 400", err)
	}
}

func TestStreamStoresReportsOnlyForItsOwnHost(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "host-1")
	other := sampleReport
	other.Name = "host-2"
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &other}); !strings.Contains(ack.Error, "host-2") {
		t.Errorf("another host's report: ack %+v", ack)
	}
	if ack := b.call(hostclient.Frame{Type: "nonsense"}); ack.Error == "" {
		t.Errorf("unknown frame: ack %+v", ack)
	}
	rep := sampleReport
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if bs := hosts(t, d); len(bs) != 1 || bs[0].Report.Name != "host-1" || len(bs[0].Workspaces) != 3 {
		t.Fatalf("hosts = %+v", bs)
	}
}

func TestStreamStoresTranscriptsAndHellosWithCursors(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	first := batch(true, "ep1", tentry(1, `{"type":"user_message","text":"fix the <b>login</b>"}`),
		tentry(2, `{"type":"assistant_message","text":"on it"}`))
	if ack := b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &first}); ack.Error != "" || ack.Code != "" {
		t.Fatalf("ack %+v", ack)
	}
	other := batch(false, "ep9", tentry(3, `{"type":"reasoning","text":"x"}`))
	if ack := b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &other}); ack.Code != hostclient.AckResend {
		t.Errorf("append from another epoch: ack %+v, want resend", ack)
	}
	ghost := batch(true, "ep1", tentry(1, `{"type":"user_message","text":"x"}`))
	ghost.AgentID = "ghost"
	if ack := b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &ghost}); ack.Code != hostclient.AckNotYet {
		t.Errorf("unknown agent: ack %+v, want not_yet", ack)
	}
	stray := first
	stray.Host = "host-2"
	if ack := b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &stray}); ack.Error == "" {
		t.Errorf("another host's transcript: ack %+v", ack)
	}
	again := openStream(t, h, "host-1")
	if c := again.hello.Cursors[agentA1]; c.Epoch != "ep1" || c.LastSeqStart != 2 {
		t.Errorf("hello = %+v", again.hello)
	}
}

func TestStreamStoresUsage(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "host-1") // before the host's first report: usage does not wait for it
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rep := usage.Report{CollectedAt: at, Sources: []usage.Source{{Provider: usage.Claude, Accounts: []usage.Account{
		{ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: true, ObservedAt: at,
			Windows: []usage.Window{{ID: "7d", Seconds: usage.Week, UsedPct: 17, ResetsAt: at.Add(72 * time.Hour)}}}}}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.UsageFrame, Usage: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var used float64
	var host string
	if err := d.Owner.QueryRow(context.Background(), `SELECT r.used_pct, a.host FROM usage_readings r
		JOIN usage_active a USING (provider, account) WHERE r.account = 'ada@example.com'`).Scan(&used, &host); err != nil || used != 17 || host != "host-1" {
		t.Errorf("stored %v on %q: %v", used, host, err)
	}
	if ack := b.call(hostclient.Frame{Type: hostclient.UsageFrame}); ack.Error == "" {
		t.Errorf("an empty usage frame was accepted")
	}
}

func TestAHostIsOnlineWhileItsStreamIsOpen(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "host-1")
	rep := sampleReport
	b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep})
	if bs := hosts(t, d); len(bs) != 1 || !bs[0].Online {
		t.Fatalf("with its stream open: %+v", bs)
	}
	b.conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for hosts(t, d)[0].Online {
		if time.Now().After(deadline) {
			t.Fatal("still online after its stream closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestANewStreamEndsTheOldOneOnAnyInstance: a host that dials again, even to another server
// instance, takes over; the old stream ends without taking the host offline.
func TestANewStreamEndsTheOldOneOnAnyInstance(t *testing.T) {
	d := dbtest.New(t)
	one := newServer(t, d, d.Owner, "v")
	two := newServer(t, d, dbtest.Pool(t, d), "v")
	old := openStream(t, one, "host-1")
	openStream(t, two, "host-1")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if _, err := hostclient.ReadFrame(ctx, old.conn); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the old stream never ended")
			}
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if bs := hosts(t, d); len(bs) != 1 || !bs[0].Online {
		t.Fatalf("after the old stream ended: %+v", bs)
	}
}

func TestStreamStoresHostMinutes(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "host-1")
	m := time.Now().UTC().Truncate(time.Minute)
	rep := resources.Report{Minutes: []resources.Minute{{Minute: m, Samples: 12, MemTotal: 100, MemAvailableMin: 40}},
		Agents: []resources.AgentMinute{{AgentID: "a1", Minute: m, MemMax: 5}}, Probe: &resources.ProbeStatus{Running: true}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var n int
	if err := d.Owner.QueryRow(context.Background(), "SELECT count(*) FROM host_minutes WHERE host = 'host-1'").Scan(&n); err != nil || n != 1 {
		t.Errorf("host minutes = %d, %v", n, err)
	}
}
