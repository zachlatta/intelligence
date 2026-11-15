package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestHostCallKeepsCommandsBeforeAck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f, err := hostclient.ReadFrame(r.Context(), conn)
		if err != nil {
			return
		}
		_ = hostclient.WriteFrame(r.Context(), conn, hostclient.Frame{Type: hostclient.CommandFrame, Command: &hostclient.Command{ID: "queued"}})
		_ = hostclient.WriteFrame(r.Context(), conn, hostclient.Frame{Type: hostclient.AckFrame, ID: f.ID})
	}))
	defer srv.Close()
	conn, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	b := &rawHost{t: t, conn: conn}
	b.call(hostclient.Frame{Type: hostclient.ReportFrame})
	if len(b.commands) != 1 {
		t.Fatal("report acknowledgment discarded a continuation command")
	}
	if c := b.command(); c.ID != "queued" {
		t.Fatal(c)
	}
}

// Stall tests use real SQL and HTTP writes, but drive host frames, command claims and
// lifecycle passes synchronously. There is no ticker or socket delivery to race the fake clock.
func newStallServer(t *testing.T, d dbtest.DB, p *pgxpool.Pool, v string, mods ...func(*Config)) *testServer {
	t.Helper()
	cfg := Config{Context: t.Context(), Version: v, DB: p, Readonly: d.Readonly, Now: func() time.Time { return t0 }}
	for _, mod := range mods {
		mod(&cfg)
	}
	f := fixtureFor(t, d)
	local := &app{cfg: cfg, store: f.store, reg: fleet.New(p), wf: workflow.NewEngine(p, cfg.Now)}
	return &testServer{Handler: newHandler(cfg, false), d: d, fixture: f, local: local}
}

func stallReport(t *testing.T, h http.Handler, rep hostclient.Report) *rawHost {
	t.Helper()
	ts := h.(*testServer)
	ts.keyFor(t, rep.Name)
	id := newStreamID()
	if err := ts.local.reg.Attach(t.Context(), rep.Name, id, ts.local.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	b := &rawHost{t: t, local: ts.local, streamID: id}
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	return b
}
