package hostwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostwatch"
)

// recorder keeps every notice the watcher sends.
type recorder struct {
	mu   sync.Mutex
	sent []hostwatch.Notice
	fail error
}

func (r *recorder) Notify(_ context.Context, n hostwatch.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, n)
	return nil
}

func (r *recorder) notices() []hostwatch.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hostwatch.Notice(nil), r.sent...)
}

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) // 07:00 in New York

type fixture struct {
	t    *testing.T
	d    dbtest.DB
	user string
	flow string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	f := &fixture{t: t, d: d}
	ctx := context.Background()
	for _, h := range []string{"box-busy", "box-idle"} {
		f.exec(`INSERT INTO hosts (name, last_seen) VALUES ($1, $2)`, h, t0)
		dbtest.Reader(t, d, h)
	}
	if err := d.Owner.QueryRow(ctx, `SELECT a.username FROM accounts a JOIN host_keys k ON k.account_id = a.id WHERE k.host = 'box-busy'`).Scan(&f.user); err != nil {
		t.Fatal(err)
	}
	// box-idle belongs to the same account, so only its lack of runs keeps it quiet
	f.exec(`UPDATE host_keys SET account_id = (SELECT account_id FROM host_keys WHERE host = 'box-busy') WHERE host = 'box-idle'`)
	if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows (account_id, name, version, created_at, updated_at)
		SELECT account_id, 'Task', 1, now(), now() FROM host_keys WHERE host = 'box-busy' RETURNING id`).Scan(&f.flow); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO workflow_versions (workflow_id, version, account_id, definition, created_at)
		SELECT $1, 1, account_id, '{"trigger":"task"}', now() FROM host_keys WHERE host = 'box-busy'`, f.flow)
	for _, st := range []string{"running", "waiting", "queued", "succeeded"} {
		f.exec(`INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status, created_at, updated_at)
			SELECT account_id, $1, 1, 'box-busy', 'Run', 'do it', $2, $3, $3 FROM host_keys WHERE host = 'box-busy'`, f.flow, st, t0.Add(-time.Hour))
	}
	// the probe saw the box run out of memory just before it went quiet, and once long before
	f.exec(`INSERT INTO oom_kills (host, at, pid, comm) VALUES ('box-busy', $1, 10, 'ffmpeg'), ('box-busy', $2, 11, 'python3'),
		('box-busy', $3, 12, 'old')`, t0.Add(-3*time.Minute), t0.Add(-time.Minute), t0.Add(-5*time.Hour))
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Owner.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) watcher(n hostwatch.Notifier, now *time.Time) *hostwatch.Watcher {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		f.t.Fatal(err)
	}
	return &hostwatch.Watcher{DB: f.d.Owner, Notify: n, Account: f.user, Location: ny,
		Started: t0.Add(-24 * time.Hour), Now: func() time.Time { return *now }}
}

func TestSilentHostWithRunsAlertsOnceAndRecoversOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rec := &recorder{}
	now := t0.Add(9 * time.Minute)
	w := f.watcher(rec, &now)

	check := func() {
		t.Helper()
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if got := rec.notices(); len(got) != 0 {
		t.Fatalf("9 minutes is not silent yet: %+v", got)
	}

	now = t0.Add(10*time.Minute + time.Second)
	check()
	got := rec.notices()
	if len(got) != 1 {
		t.Fatalf("want one alert, got %+v", got)
	}
	a := got[0]
	t.Logf("alert: %s | %s", a.Title, a.Body)
	for _, want := range []string{"box-busy"} {
		if !strings.Contains(a.Title, want) {
			t.Errorf("title %q lacks %q", a.Title, want)
		}
	}
	for _, want := range []string{"10 min", "07:00", "3 runs", "1 running", "1 waiting", "1 queued", "Last recorded: 2 OOM kills up to 06:59", "python3", "ffmpeg"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body %q lacks %q", a.Body, want)
		}
	}
	if strings.Contains(a.Body, "old") {
		t.Errorf("body %q names an OOM kill from hours before", a.Body)
	}

	// it stays down: no repeat, however long, and box-idle (silent too, no runs) never alerts
	for _, d := range []time.Duration{11 * time.Minute, 30 * time.Minute, 3 * time.Hour} {
		now = t0.Add(d)
		check()
	}
	if got := rec.notices(); len(got) != 1 {
		t.Fatalf("a host that stays down alerts once: %+v", got)
	}
	var open int
	if err := f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM host_silence_alerts WHERE host = 'box-idle'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("idle host alerts: %d %v", open, err)
	}

	// a second server instance (or a restart) sees the alert in Postgres and does not repeat it
	other := f.watcher(rec, &now)
	other.DB = dbtest.Pool(t, f.d)
	if err := other.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.notices(); len(got) != 1 {
		t.Fatalf("a restart repeats the alert: %+v", got)
	}

	// it reports again: one recovery notice
	back := t0.Add(3*time.Hour + 5*time.Minute)
	f.exec(`UPDATE hosts SET last_seen = $1 WHERE name = 'box-busy'`, back)
	now = back.Add(30 * time.Second)
	check()
	check()
	got = rec.notices()
	if len(got) != 2 {
		t.Fatalf("want alert and one recovery, got %+v", got)
	}
	r := got[1]
	t.Logf("recovery: %s | %s", r.Title, r.Body)
	if !strings.Contains(r.Title, "box-busy") || !strings.Contains(r.Body, "3 h 5 min") || !strings.Contains(r.Body, "10:05") {
		t.Errorf("recovery %+v", r)
	}

	// the state is readable with read-only SQL, as the owning account
	reader := dbtest.Reader(t, f.d, "box-busy")
	var host string
	var sent, recovered, recoverySent bool
	if err := reader.QueryRow(ctx, `SELECT host, alert_sent_at IS NOT NULL, recovered_at IS NOT NULL, recovery_sent_at IS NOT NULL
		FROM host_silence_alerts`).Scan(&host, &sent, &recovered, &recoverySent); err != nil {
		t.Fatal(err)
	}
	if host != "box-busy" || !sent || !recovered || !recoverySent {
		t.Fatalf("row: %s %v %v %v", host, sent, recovered, recoverySent)
	}
	var banners int
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM host_silence_current`).Scan(&banners); err != nil || banners != 0 {
		t.Fatalf("a recovered host still shows: %d %v", banners, err)
	}
	if err := db.CheckContract(ctx, f.d.Owner, f.d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestBannerShowsOpenAlertToItsAccountOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := t0.Add(25 * time.Minute)
	if err := f.watcher(&recorder{}, &now).Check(ctx); err != nil {
		t.Fatal(err)
	}
	reader := dbtest.Reader(t, f.d, "box-busy")
	var host string
	var runs, kills int
	if err := reader.QueryRow(ctx, `SELECT host, active_runs, oom_kills FROM host_silence_current`).Scan(&host, &runs, &kills); err != nil {
		t.Fatal(err)
	}
	if host != "box-busy" || runs != 3 || kills != 2 {
		t.Fatalf("banner row: %s %d %d", host, runs, kills)
	}
	stranger := dbtest.Reader(t, f.d, "someone-elses-box")
	var n int
	if err := stranger.QueryRow(ctx, `SELECT count(*) FROM host_silence_alerts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("another account reads the alert: %d %v", n, err)
	}
}

// The probe often cannot upload while the host is wedged, so the kills it last managed to record
// can be hours old: they still say what the host went through.
func TestAlertNamesTheLastOOMKillsHoweverOld(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM oom_kills WHERE comm <> 'old'`)
	rec := &recorder{}
	now := t0.Add(11 * time.Minute)
	if err := f.watcher(rec, &now).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := rec.notices()
	if len(got) != 1 || !strings.Contains(got[0].Body, "Last recorded: 1 OOM kill up to Nov 19 02:00") || !strings.Contains(got[0].Body, "(old)") {
		t.Fatalf("%+v", got)
	}
	f.exec(`DELETE FROM oom_kills`)
	f.exec(`DELETE FROM host_silence_alerts`)
	rec = &recorder{}
	if err := f.watcher(rec, &now).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.notices(); len(got) != 1 || !strings.Contains(got[0].Body, "No OOM kills recorded.") {
		t.Fatalf("%+v", got)
	}
}

func TestServerStartIsNotSilence(t *testing.T) {
	f := setup(t)
	rec := &recorder{}
	now := t0.Add(time.Hour)
	w := f.watcher(rec, &now)
	w.Started = now.Add(-2 * time.Minute) // the server was down itself; hosts have not dialed back yet
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.notices(); len(got) != 0 {
		t.Fatalf("alerted right after a server start: %+v", got)
	}
}

func TestFailedSendIsRetriedNotLost(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rec := &recorder{fail: errors.New("PDW HTTP 502")}
	now := t0.Add(11 * time.Minute)
	w := f.watcher(rec, &now)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	var msg string
	if err := f.d.Owner.QueryRow(ctx, `SELECT alert_error FROM host_silence_alerts`).Scan(&msg); err != nil || !strings.Contains(msg, "502") {
		t.Fatalf("error not kept: %q %v", msg, err)
	}
	rec.mu.Lock()
	rec.fail = nil
	rec.mu.Unlock()
	now = now.Add(time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.notices(); len(got) != 1 {
		t.Fatalf("want the alert once after a retry, got %+v", got)
	}
}

func TestOtherAccountsHostsGetBannerButNoPush(t *testing.T) {
	f := setup(t)
	rec := &recorder{}
	now := t0.Add(15 * time.Minute)
	w := f.watcher(rec, &now)
	w.Account = "someone-else"
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.notices(); len(got) != 0 {
		t.Fatalf("pushed for a host outside the notified account: %+v", got)
	}
	var n int
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM host_silence_alerts WHERE NOT notify`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("banner state: %d %v", n, err)
	}
}

func TestPDWSendsThroughNotifyTool(t *testing.T) {
	var got struct {
		path, auth string
		body       map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = w.Write([]byte(`{"data":{"report":{"sent":1}}}`))
	}))
	defer srv.Close()
	p := &hostwatch.PDW{URL: srv.URL, Key: "k", Client: srv.Client()}
	if err := p.Notify(context.Background(), hostwatch.Notice{Title: "t", Body: "b", ThreadID: "x", Level: "time-sensitive"}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/tools/notify" || got.auth != "Bearer intelligence:k" || got.body["title"] != "t" || got.body["body"] != "b" ||
		got.body["thread_id"] != "x" || got.body["interruption_level"] != "time-sensitive" {
		t.Fatalf("%+v", got)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"report":{"sent":0},"error":"no devices"}}`))
	}))
	defer failing.Close()
	p = &hostwatch.PDW{URL: failing.URL, Key: "k", Client: failing.Client()}
	if err := p.Notify(context.Background(), hostwatch.Notice{Title: "t"}); err == nil || !strings.Contains(err.Error(), "no devices") {
		t.Fatalf("tool error not surfaced: %v", err)
	}
}
