package web

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/timing"
)

// timedServer is a test server whose requests are recorded to rec, which the test flushes.
func timedServer(t *testing.T) (*testServer, *timing.Recorder) {
	t.Helper()
	d := dbtest.New(t)
	rec := timing.New(d.Owner, 1000)
	return newServer(t, d, d.Owner, "test", func(c *Config) { c.Timing = rec }), rec
}

type requestRow struct {
	method, route string
	status        int
	ms            float64
	account       *int64
}

func requestRows(t *testing.T, h *testServer, rec *timing.Recorder) []requestRow {
	t.Helper()
	if err := rec.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := h.d.Owner.Query(t.Context(), `SELECT method, route, status, duration_ms, account_id FROM http_requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []requestRow
	for rows.Next() {
		var r requestRow
		if err := rows.Scan(&r.method, &r.route, &r.status, &r.ms, &r.account); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

// Every request is recorded with the route pattern that served it, never the path, query or any
// credential, and with the account that made it when someone was signed in.
func TestEveryRequestIsTimed(t *testing.T) {
	t.Parallel()
	h, rec := timedServer(t)
	get(t, h, "/workflow-runs/0b6c-secret-id?token=hunter2", map[string]string{"Authorization": "Bearer sekrit"})
	call(t, h, http.MethodPost, "/api/sql", `{"query":"SELECT 'hunter2'"}`, sameOriginFetch)
	get(t, anon(h), "/login?next=%2Fprivate", nil)
	get(t, anon(h), "/no/such/page", nil)
	got := requestRows(t, h, rec)
	if len(got) != 4 {
		t.Fatalf("recorded %d requests, want 4: %+v", len(got), got)
	}
	want := []struct {
		method, route string
		signedIn      bool
	}{
		{"GET", "/workflow-runs/{id}", true},
		{"POST", "/api/sql", true},
		{"GET", "/login", false},
		{"GET", "(no route)", false},
	}
	for i, w := range want {
		r := got[i]
		if r.method != w.method || r.route != w.route || r.status == 0 || r.ms < 0 || (r.account != nil) != w.signedIn {
			t.Errorf("request %d = %+v, want %s %s signed in %v", i, r, w.method, w.route, w.signedIn)
		}
		if w.signedIn && *r.account != h.acct.ID {
			t.Errorf("request %d account = %d, want %d", i, *r.account, h.acct.ID)
		}
	}
	if got[2].status != http.StatusOK || got[3].status != http.StatusNotFound {
		t.Errorf("statuses %d, %d; want 200, 404", got[2].status, got[3].status)
	}
	var leaked int
	if err := h.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM http_requests t WHERE row_to_json(t)::text ~ '(secret|hunter2|sekrit|private|next|token)'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("%d rows hold request data, %v", leaked, err)
	}
}

// The account reads its own requests, and those no one was signed in for, through the SQL API;
// another account sees none of the first account's.
func TestRequestTimingIsReadableBySQL(t *testing.T) {
	t.Parallel()
	h, rec := timedServer(t)
	get(t, h, "/workflows", nil)
	get(t, anon(h), "/login", nil)
	if err := rec.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := call(t, h, http.MethodPost, "/api/sql", `{"query":"SELECT route, percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) >= 0 FROM http_requests WHERE at > now() - interval '24 hours' GROUP BY route ORDER BY route"}`, sameOriginFetch)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"/login"`) || !strings.Contains(res.Body.String(), `"/workflows"`) {
		t.Fatalf("SQL: %d %s", res.Code, res.Body)
	}
	other, err := h.store.Create(t.Context(), "other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	res = call(t, h, http.MethodPost, "/api/sql", `{"query":"SELECT route FROM http_requests WHERE account_id IS NOT NULL"}`, sameOriginFetch, sessionFor(t, h.store, other))
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "/workflows") {
		t.Errorf("another account read the first's requests: %d %s", res.Code, res.Body)
	}
}

// A page's own load timing is stored with the route pattern of the page, not its path, and is
// readable by its account through the SQL API.
func TestPageLoadBeacon(t *testing.T) {
	t.Parallel()
	h, _ := timedServer(t)
	body := `{"path":"/workflow-runs/abc123?x=secret","navigationType":"navigate","ttfbMs":12.5,"domContentLoadedMs":80,"loadMs":120,"durationMs":121.25}`
	if res := call(t, h, http.MethodPost, "/api/page-loads", body, sameOriginFetch); res.Code != http.StatusNoContent {
		t.Fatalf("beacon: %d %s", res.Code, res.Body)
	}
	// a value past the bound is clamped, not refused
	big := `{"path":"/","navigationType":"reload","ttfbMs":1,"domContentLoadedMs":2,"loadMs":3,"durationMs":99999999}`
	if res := call(t, h, http.MethodPost, "/api/page-loads", big, sameOriginFetch); res.Code != http.StatusNoContent {
		t.Fatalf("big beacon: %d %s", res.Code, res.Body)
	}
	res := call(t, h, http.MethodPost, "/api/sql", `{"query":"SELECT route, navigation_type, ttfb_ms, dom_content_loaded_ms, load_ms, duration_ms FROM page_loads ORDER BY id"}`, sameOriginFetch)
	if res.Code != http.StatusOK {
		t.Fatalf("SQL: %d %s", res.Code, res.Body)
	}
	for _, want := range []string{`["/workflow-runs/{id}","navigate",12.5,80,120,121.25]`, `["/{$}","reload",1,2,3,600000]`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Errorf("SQL result lacks %s: %s", want, res.Body)
		}
	}
	if strings.Contains(res.Body.String(), "secret") || strings.Contains(res.Body.String(), "abc123") {
		t.Errorf("stored the raw path: %s", res.Body)
	}
}

// The beacon is a write like any other: signed in, same origin, JSON, small and well formed.
func TestPageLoadBeaconRefuses(t *testing.T) {
	t.Parallel()
	h, _ := timedServer(t)
	good := `{"path":"/","navigationType":"navigate","ttfbMs":1,"domContentLoadedMs":2,"loadMs":3,"durationMs":3}`
	for _, tc := range []struct {
		name   string
		h      http.Handler
		body   string
		header map[string]string
		want   int
	}{
		{"signed out", anon(h), good, sameOriginFetch, http.StatusUnauthorized},
		{"cross-site", h, good, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"negative", h, strings.Replace(good, `"ttfbMs":1`, `"ttfbMs":-1`, 1), sameOriginFetch, http.StatusBadRequest},
		{"no path", h, strings.Replace(good, `"path":"/"`, `"path":"https://elsewhere/"`, 1), sameOriginFetch, http.StatusBadRequest},
		{"bad navigation type", h, strings.Replace(good, `"navigate"`, `"teleport"`, 1), sameOriginFetch, http.StatusBadRequest},
		{"unknown field", h, strings.Replace(good, `{`, `{"userAgent":"x",`, 1), sameOriginFetch, http.StatusBadRequest},
		{"too big", h, strings.Replace(good, `"path":"/"`, `"path":"/`+strings.Repeat("a", 9000)+`"`, 1), sameOriginFetch, http.StatusBadRequest},
	} {
		if res := call(t, tc.h, http.MethodPost, "/api/page-loads", tc.body, tc.header); res.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, res.Code, res.Body, tc.want)
		}
	}
	var n int
	if err := h.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM page_loads`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d page loads stored, %v", n, err)
	}
}

// Every page loads pageload.js.
func TestLayoutLoadsPageLoadScript(t *testing.T) {
	t.Parallel()
	if !strings.Contains(layoutHTML, `<script src="/pageload.js" defer></script>`) {
		t.Error("layout.html does not load /pageload.js")
	}
	h, _ := timedServer(t)
	res := get(t, anon(h), "/pageload.js", nil)
	if res.Code != http.StatusOK || !strings.HasPrefix(res.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("GET /pageload.js: %d %s", res.Code, res.Header().Get("Content-Type"))
	}
	page := get(t, h, "/workflows", nil)
	if !strings.Contains(page.Body.String(), `/pageload.js`) {
		t.Error("the workflows page does not load /pageload.js")
	}
}

// pageLoadHarness runs pageload.js (argv[1]) with a fake navigation entry and fetch, fires the
// load event (if argv[2] is "load"), and prints what it posted.
const pageLoadHarness = `
const fs = require("fs");
const [file, mode] = process.argv.slice(1);
const listeners = {};
global.window = global;
global.addEventListener = (k, f) => { (listeners[k] = listeners[k] || []).push(f); };
global.document = { readyState: mode === "loading" ? "loading" : "complete", querySelector: (sel) => sel === "nav.topnav" && mode !== "signedout" ? {} : null };
global.location = { pathname: "/workflow-runs/42", search: "?q=secret" };
global.performance = { getEntriesByType: (k) => k === "navigation" ? [{ type: "navigate", startTime: 0, requestStart: 3, responseStart: 12.5, domContentLoadedEventEnd: 80.4, loadEventEnd: mode === "none" ? 0 : 120, duration: 121 }] : [] };
const posts = [];
global.fetch = (url, opts) => { posts.push({ url, method: opts.method, keepalive: opts.keepalive, type: opts.headers["Content-Type"], body: JSON.parse(opts.body) }); return Promise.resolve({ ok: true }); };
eval(fs.readFileSync(file, "utf8"));
if (mode === "load") for (const f of listeners.load || []) f();
setTimeout(() => console.log(JSON.stringify(posts)), 20);
`

// After the page loads, pageload.js posts its navigation timing once, with its path but not its
// query; a page still loading posts nothing until its load event, and a signed-out page nothing.
func TestPageLoadScript(t *testing.T) {
	t.Parallel()
	node := requireNode(t)
	want := `[{"url":"/api/page-loads","method":"POST","keepalive":true,"type":"application/json","body":{"path":"/workflow-runs/42","navigationType":"navigate","ttfbMs":12.5,"domContentLoadedMs":80.4,"loadMs":120,"durationMs":121}}]`
	for _, tc := range []struct{ mode, want string }{
		{"load", want},
		{"complete", want},
		{"loading", `[]`},
		{"signedout", `[]`},
	} {
		out, err := exec.Command(node, "-e", pageLoadHarness, "pageload.js", tc.mode).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("%s: %v\n got %s\nwant %s", tc.mode, err, strings.TrimSpace(string(out)), tc.want)
		}
	}
}
