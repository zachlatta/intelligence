package web

// Spec tests (verifier rhea) for the Grimm header's live host resource overview: a compact
// widget in the upper right of a session's header, for the session's host, read through SQL,
// updated live through the session's stream, with a detail card on hover or tap.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/probe"
	"github.com/zachlatta/intelligence/internal/resources"
)

// verifyHostReport is one finished minute of a busy host, with distinctive numbers the tests
// look for: CPU avg cpu and peak 93.5, load 6.25 on 8 CPUs, 3 of 32 GiB memory free, 2 GiB of
// swap used, two filesystems (one 91% full), disk I/O, pressure, two OOM kills, the Paseo cgroup
// at 7 GiB, and two agents: the session's worker w-1 and an agent of no session.
func verifyHostReport(m time.Time, cpu float64) resources.Report {
	f := func(v float64) *float64 { return &v }
	u := func(v uint64) *uint64 { return &v }
	return resources.Report{
		Minutes: []resources.Minute{{Minute: m, Samples: 12, CPUs: 8, CPUAvgPct: f(cpu), CPUMaxPct: f(93.5),
			Load1: 6.25, Load5: 5.5, Load15: 4.75,
			MemTotal: 32 << 30, MemAvailableMin: 3 << 30, SwapTotal: 8 << 30, SwapUsedMax: 2 << 30,
			DiskTotal: 100 << 30, DiskAvailableMin: 9 << 30,
			DiskReadBps: f(21 << 20), DiskWriteBps: f(5 << 20), DiskIOPS: f(321), DiskReadBpsMax: f(60 << 20), DiskWriteBpsMax: f(9 << 20),
			DiskBusyAvgPct: f(71), DiskBusyMaxPct: f(98),
			PSICPUSome: f(12.5), PSIMemSome: f(3.5), PSIMemFull: f(1.5), PSIIOSome: f(22.5), PSIIOFull: f(8.5),
			OOMKills: u(2), PaseoMemMax: u(7 << 30), PaseoMemPeak: u(9 << 30), PaseoOOMKills: u(1),
			Filesystems: []resources.Filesystem{
				{Mount: "/", Device: "/dev/sda1", Type: "ext4", Total: 100 << 30, Available: 9 << 30},
				{Mount: "/srv/verify-data", Device: "/dev/sdb1", Type: "xfs", Total: 2000 << 30, Available: 1500 << 30}}}},
		Agents: []resources.AgentMinute{
			{AgentID: "w-1", Minute: m, CPUSeconds: f(30), CPUSource: "ebpf", MemMax: 5 << 30, ProcessesMax: 6, Execs: u(40)},
			{AgentID: "stranger-agent", Minute: m, CPUSeconds: f(55), CPUSource: "ebpf", MemMax: 6 << 30, ProcessesMax: 3, Execs: u(9)}},
		OOMs:  []probe.OOM{{At: m, PID: 4242, Comm: "verify-node", AgentID: "w-1", AnonRSS: 4 << 30}},
		Probe: &resources.ProbeStatus{Running: true, Version: "v", Tracked: 12},
	}
}

func recordVerifyMinute(t *testing.T, h http.Handler, host string, m time.Time, cpu float64) {
	t.Helper()
	d := h.(*testServer).d
	if err := fleet.New(d.Owner).RecordHost(t.Context(), host, verifyHostReport(m, cpu), m); err != nil {
		t.Fatal(err)
	}
}

// gBar cuts the g-bar header out of a session page, failing the test when it is missing.
func gBar(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `<header class="g-bar"`)
	if start < 0 {
		t.Fatalf("no g-bar header:\n%s", page)
	}
	end := strings.Index(page[start:], "</header>")
	if end < 0 {
		t.Fatal("g-bar header not closed")
	}
	return page[start : start+end]
}

// The widget is in the session header, to the right of the spacer (the upper right), and the
// header keeps polly's worker-cap control.
func TestVerifyGrimmHostWidgetSitsInTheHeadersUpperRight(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	page := get(t, h, "/grimm/"+id, nil).Body.String()
	bar := gBar(t, page)
	host := regexp.MustCompile(`id="g-host"`).FindStringIndex(bar)
	spacer := strings.Index(bar, `g-spacer`)
	if host == nil {
		t.Fatalf("the header has no #g-host widget:\n%s", bar)
	}
	if spacer < 0 || host[0] < spacer {
		t.Errorf("#g-host is not right of the header's spacer (upper right):\n%s", bar)
	}
	if !strings.Contains(bar, `id="g-cap"`) {
		t.Errorf("the header lost the worker-cap control #g-cap:\n%s", bar)
	}
	for _, asset := range []string{"/grimm_host.js", "/grimm_host.css"} {
		if !strings.Contains(page, asset) {
			t.Errorf("the page does not load %s", asset)
			continue
		}
		rec := get(t, h, asset, nil)
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s: %d (%d bytes)", asset, rec.Code, rec.Body.Len())
		}
	}
	// The home page with no session has no session host to show and must still render.
	if rec := get(t, h, "/?new=1", nil); rec.Code != 200 {
		t.Errorf("new-session page: %d", rec.Code)
	}
}

// The widget's script refreshes through the stream, not a fast timer: no setInterval or
// setTimeout loop that re-reads under 15 seconds. (A once-a-second timer that only re-renders
// the reading's age is allowed; it must not fetch.)
func TestVerifyGrimmHostWidgetDoesNotPollFast(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	js := get(t, h, "/grimm_host.js", nil).Body.String()
	if js == "" {
		t.Fatal("no /grimm_host.js")
	}
	if !strings.Contains(js, "host_resources") && !strings.Contains(js, "grimm:host-resources") {
		t.Error("the widget does not listen to the stream's host_resources event")
	}
	if !strings.Contains(js, "/api/sql") {
		t.Error("the widget does not read through /api/sql")
	}
	// Any timer callback that fetches is polling.
	timer := regexp.MustCompile(`set(Interval|Timeout)\(`)
	for _, loc := range timer.FindAllStringIndex(js, -1) {
		window := js[loc[0]:min(len(js), loc[0]+400)]
		if strings.Contains(window, "fetch(") || strings.Contains(window, "/api/sql") {
			t.Errorf("a timer fetches (polling):\n%s", window)
		}
	}
}

// Everything the widget shows is one readable relation for the session: the newest minute of
// the session's host with the detail the card shows, and only this session's agents.
func TestVerifyGrimmHostResourcesReadThroughSQL(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	id, _ := readySession(t, h)
	now := time.Now().UTC().Truncate(time.Minute)
	recordVerifyMinute(t, h, "host-1", now.Add(-2*time.Minute), 11.5)
	recordVerifyMinute(t, h, "host-1", now.Add(-time.Minute), 41.5)
	recordVerifyMinute(t, h, "host-9", now.Add(-time.Minute), 99.25) // another host, not the session's

	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatalf("CheckContract: %v", err)
	}
	code, r := sqlPost(t, h, q(`SELECT row_to_json(x)::text FROM grimm_host_resources x WHERE session_id = '`+id+`'`))
	if code != 200 || r.Error != "" {
		t.Fatalf("grimm_host_resources: %d %s", code, r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(r.Rows), r.Rows)
	}
	row, _ := r.Rows[0][0].(string)
	for _, want := range []string{
		`"host-1"`,         // the session's host
		"41.5",             // the newest minute's CPU average, not the older 11.5
		"93.5",             // CPU peak
		"6.25",             // load 1
		"0.78125",          // load per CPU (6.25 / 8)
		"/srv/verify-data", // filesystems
		"athena",           // this session's worker, by name
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the row misses %q:\n%s", want, row)
		}
	}
	for _, bad := range []string{"99.25", "stranger-agent"} {
		if strings.Contains(row, bad) {
			t.Errorf("the row has %q (another host's or another session's):\n%s", bad, row)
		}
	}
	// The detail's numbers are in the row or in host_current/host_filesystems for the host.
	code, r = sqlPost(t, h, q(`SELECT row_to_json(c)::text FROM host_current c WHERE host = 'host-1'`))
	if code != 200 || len(r.Rows) != 1 {
		t.Fatalf("host_current: %d %s", code, r.Error)
	}
	// A second account cannot read this session's row.
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "otherhostviewer", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, other)
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.AddCookie(cookie); anon(h).ServeHTTP(w, r) })
	code, r = sqlPost(t, otherH, q(`SELECT count(*)::int FROM grimm_host_resources`))
	if code != 200 || r.Error != "" || len(r.Rows) != 1 {
		t.Fatalf("another account's count: %d %s", code, r.Error)
	}
	if n, _ := r.Rows[0][0].(float64); n != 0 {
		t.Errorf("another account reads %v grimm_host_resources rows, want 0", r.Rows[0][0])
	}
}

// A session whose host has never reported still has a row (the widget renders "no reading").
func TestVerifyGrimmHostResourcesRowWithoutAReading(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	code, r := sqlPost(t, h, q(`SELECT count(*)::int FROM grimm_host_resources WHERE session_id = '`+id+`'`))
	if code != 200 || r.Error != "" || len(r.Rows) != 1 {
		t.Fatalf("%d %s %+v", code, r.Error, r.Rows)
	}
	if n, _ := r.Rows[0][0].(float64); n != 1 {
		t.Errorf("a session with no host reading has %v rows, want 1", r.Rows[0][0])
	}
}

// A stale reading (the newest minute older than three minutes) is marked as such in what the
// widget reads, so the page can say so.
func TestVerifyGrimmHostResourcesMarkAStaleReading(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	recordVerifyMinute(t, h, "host-1", time.Now().UTC().Truncate(time.Minute).Add(-10*time.Minute), 41.5)
	code, r := sqlPost(t, h, q(`SELECT row_to_json(x)::text FROM grimm_host_resources x WHERE session_id = '`+id+`'`))
	if code != 200 || len(r.Rows) != 1 {
		t.Fatalf("%d %s", code, r.Error)
	}
	row, _ := r.Rows[0][0].(string)
	var m map[string]any
	if err := json.Unmarshal([]byte(row), &m); err != nil {
		t.Fatal(err)
	}
	fresh, hasFresh := m["fresh"]
	_, hasMinute := m["minute"]
	if !(hasFresh && fresh == false) && !hasMinute {
		t.Errorf("a 10-minute-old reading is neither marked fresh=false nor carries its minute for the page to age:\n%s", row)
	}
}

// The session's stream sends host_resources, without a reload, when the session's host
// records a new minute; another host's minute sends nothing.
func TestVerifyGrimmStreamPushesTheHostsNewMinute(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	now := time.Now().UTC().Truncate(time.Minute)
	recordVerifyMinute(t, h, "host-1", now.Add(-2*time.Minute), 11.5)
	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+id+"/stream")
	defer cancel()
	next := func(within time.Duration) (string, bool) {
		t.Helper()
		deadline := time.After(within)
		for {
			select {
			case e, open := <-events:
				if !open {
					t.Fatal("the stream ended")
				}
				if e[0] == "host_resources" {
					return e[1], true
				}
			case <-deadline:
				return "", false
			}
		}
	}
	// An event when the stream opens is allowed (the page reads on load either way); drain it.
	_, opened := next(3 * time.Second)
	t.Logf("host_resources on open: %v", opened)
	recordVerifyMinute(t, h, "host-9", now.Add(-time.Minute), 99.25)
	if data, ok := next(2 * time.Second); ok {
		t.Errorf("another host's minute sent host_resources: %s", data)
	}
	start := time.Now()
	recordVerifyMinute(t, h, "host-1", now.Add(-time.Minute), 41.5)
	if _, ok := next(5 * time.Second); !ok {
		t.Fatal("the session host's new minute did not reach the stream within 5s")
	}
	t.Logf("new minute reached the stream in %s", time.Since(start))
}
