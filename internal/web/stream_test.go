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
	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/host"
	"github.com/zachlatta/intelligence/internal/usage"
)

// rawBox is a box's end of the stream, driven frame by frame.
type rawBox struct {
	t     *testing.T
	conn  *websocket.Conn
	hello devbox.Hello
	next  uint64
}

func wsURL(srv *httptest.Server, box string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + devbox.StreamPath(box)
}

// dialStream opens box's stream to h with token; the response is the refusal when err is set.
func dialStream(t *testing.T, srv *httptest.Server, box, token string) (*rawBox, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, box), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, resp, err
	}
	conn.SetReadLimit(devbox.MaxFrame)
	t.Cleanup(func() { conn.CloseNow() })
	f, err := devbox.ReadFrame(ctx, conn)
	if err != nil || f.Type != devbox.HelloFrame {
		t.Fatalf("hello = %+v, %v", f, err)
	}
	return &rawBox{t: t, conn: conn, hello: *f.Hello}, resp, nil
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func openStream(t *testing.T, h http.Handler, box string) *rawBox {
	t.Helper()
	b, _, err := dialStream(t, serve(t, h), box, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// call sends f and returns its ack, skipping any command frames.
func (b *rawBox) call(f devbox.Frame) devbox.Frame {
	b.t.Helper()
	b.next++
	f.ID = b.next
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := devbox.WriteFrame(ctx, b.conn, f); err != nil {
		b.t.Fatal(err)
	}
	for {
		got, err := devbox.ReadFrame(ctx, b.conn)
		if err != nil {
			b.t.Fatalf("waiting for the ack of %s: %v", f.Type, err)
		}
		if got.Type == devbox.AckFrame {
			if got.ID != f.ID {
				b.t.Fatalf("ack %d, want %d", got.ID, f.ID)
			}
			return got
		}
	}
}

// command reads frames until a command arrives.
func (b *rawBox) command() devbox.Command {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		f, err := devbox.ReadFrame(ctx, b.conn)
		if err != nil {
			b.t.Fatalf("waiting for a command: %v", err)
		}
		if f.Type == devbox.CommandFrame {
			return *f.Command
		}
	}
}

// sendReport sends rep on its box's stream, which stays open (so the box is online), and fails
// the test unless it is stored.
func sendReport(t *testing.T, h http.Handler, rep devbox.Report) {
	t.Helper()
	b := openStream(t, h, rep.Name)
	if ack := b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatalf("report refused: %s", ack.Error)
	}
}

// sendBatch sends a transcript batch on its box's stream and returns the ack.
func sendBatch(t *testing.T, h http.Handler, b devbox.TranscriptBatch) devbox.Frame {
	t.Helper()
	s := openStream(t, h, b.Box)
	defer s.conn.Close(websocket.StatusNormalClosure, "")
	return s.call(devbox.Frame{Type: devbox.TranscriptFrame, Transcript: &b})
}

func TestStreamNeedsTheToken(t *testing.T) {
	h, d := dbServer(t)
	srv := serve(t, h)
	for _, tok := range []string{"", "wrong"} {
		_, resp, err := dialStream(t, srv, "devbox-1", tok)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: %v, want 401", tok, err)
		}
		if resp != nil && resp.Header.Get(devbox.VersionHeader) != "v" {
			t.Errorf("a refused stream must still name the server's version, for the box to update")
		}
	}
	if len(boxes(t, d)) != 0 {
		t.Fatalf("an unauthorized box was recorded")
	}
}

func TestStreamsRefusedWhenNoTokenIsConfigured(t *testing.T) {
	d := dbtest.New(t)
	srv := serve(t, New(Config{Context: t.Context(), Version: "v", DB: d.Owner, Readonly: d.Readonly}))
	if _, resp, err := dialStream(t, srv, "devbox-1", ""); err == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%v, want 503", err)
	}
}

func TestStreamRefusesABadName(t *testing.T) {
	h, _ := dbServer(t)
	if _, resp, err := dialStream(t, serve(t, h), "Bad_Name", "secret"); err == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%v, want 400", err)
	}
}

func TestStreamStoresReportsOnlyForItsOwnBox(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "devbox-1")
	other := sampleReport
	other.Name = "devbox-2"
	if ack := b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &other}); !strings.Contains(ack.Error, "devbox-2") {
		t.Errorf("another box's report: ack %+v", ack)
	}
	if ack := b.call(devbox.Frame{Type: "nonsense"}); ack.Error == "" {
		t.Errorf("unknown frame: ack %+v", ack)
	}
	rep := sampleReport
	if ack := b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if bs := boxes(t, d); len(bs) != 1 || bs[0].Report.Name != "devbox-1" || len(bs[0].Workspaces) != 3 {
		t.Fatalf("boxes = %+v", bs)
	}
}

func TestStreamStoresTranscriptsAndHellosWithCursors(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "devbox-1")
	first := batch(true, "ep1", tentry(1, `{"type":"user_message","text":"fix the <b>login</b>"}`),
		tentry(2, `{"type":"assistant_message","text":"on it"}`))
	if ack := b.call(devbox.Frame{Type: devbox.TranscriptFrame, Transcript: &first}); ack.Error != "" || ack.Code != "" {
		t.Fatalf("ack %+v", ack)
	}
	other := batch(false, "ep9", tentry(3, `{"type":"reasoning","text":"x"}`))
	if ack := b.call(devbox.Frame{Type: devbox.TranscriptFrame, Transcript: &other}); ack.Code != devbox.AckResend {
		t.Errorf("append from another epoch: ack %+v, want resend", ack)
	}
	ghost := batch(true, "ep1", tentry(1, `{"type":"user_message","text":"x"}`))
	ghost.AgentID = "ghost"
	if ack := b.call(devbox.Frame{Type: devbox.TranscriptFrame, Transcript: &ghost}); ack.Code != devbox.AckNotYet {
		t.Errorf("unknown agent: ack %+v, want not_yet", ack)
	}
	stray := first
	stray.Box = "devbox-2"
	if ack := b.call(devbox.Frame{Type: devbox.TranscriptFrame, Transcript: &stray}); ack.Error == "" {
		t.Errorf("another box's transcript: ack %+v", ack)
	}
	again := openStream(t, h, "devbox-1")
	if c := again.hello.Cursors[agentA1]; c.Epoch != "ep1" || c.LastSeqStart != 2 {
		t.Errorf("hello = %+v", again.hello)
	}
}

func TestStreamStoresUsage(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "devbox-1") // before the box's first report: usage does not wait for it
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rep := usage.Report{CollectedAt: at, Sources: []usage.Source{{Provider: usage.Claude, Accounts: []usage.Account{
		{ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: true, ObservedAt: at,
			Windows: []usage.Window{{ID: "7d", Seconds: usage.Week, UsedPct: 17, ResetsAt: at.Add(72 * time.Hour)}}}}}}}
	if ack := b.call(devbox.Frame{Type: devbox.UsageFrame, Usage: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var used float64
	var box string
	if err := d.Owner.QueryRow(context.Background(), `SELECT r.used_pct, a.box FROM usage_readings r
		JOIN usage_active a USING (provider, account) WHERE r.account = 'ada@example.com'`).Scan(&used, &box); err != nil || used != 17 || box != "devbox-1" {
		t.Errorf("stored %v on %q: %v", used, box, err)
	}
	if ack := b.call(devbox.Frame{Type: devbox.UsageFrame}); ack.Error == "" {
		t.Errorf("an empty usage frame was accepted")
	}
}

func TestABoxIsOnlineWhileItsStreamIsOpen(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "devbox-1")
	rep := sampleReport
	b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &rep})
	if bs := boxes(t, d); len(bs) != 1 || !bs[0].Online {
		t.Fatalf("with its stream open: %+v", bs)
	}
	b.conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for boxes(t, d)[0].Online {
		if time.Now().After(deadline) {
			t.Fatal("still online after its stream closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestANewStreamEndsTheOldOneOnAnyInstance: a box that dials again, even to another server
// instance, takes over; the old stream ends without taking the box offline.
func TestANewStreamEndsTheOldOneOnAnyInstance(t *testing.T) {
	d := dbtest.New(t)
	one := New(Config{Context: t.Context(), Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret"})
	two := New(Config{Context: t.Context(), Version: "v", DB: dbtest.Pool(t, d), Readonly: d.Readonly, DevboxToken: "secret"})
	old := openStream(t, one, "devbox-1")
	openStream(t, two, "devbox-1")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if _, err := devbox.ReadFrame(ctx, old.conn); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the old stream never ended")
			}
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if bs := boxes(t, d); len(bs) != 1 || !bs[0].Online {
		t.Fatalf("after the old stream ended: %+v", bs)
	}
}

func TestStreamStoresHostMinutes(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "devbox-1")
	m := time.Now().UTC().Truncate(time.Minute)
	rep := host.Report{Minutes: []host.Minute{{Minute: m, Samples: 12, MemTotal: 100, MemAvailableMin: 40}},
		Agents: []host.AgentMinute{{AgentID: "a1", Minute: m, MemMax: 5}}, Probe: &host.ProbeStatus{Running: true}}
	if ack := b.call(devbox.Frame{Type: devbox.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var n int
	if err := d.Owner.QueryRow(context.Background(), "SELECT count(*) FROM host_minutes WHERE box = 'devbox-1'").Scan(&n); err != nil || n != 1 {
		t.Errorf("host minutes = %d, %v", n, err)
	}
}
