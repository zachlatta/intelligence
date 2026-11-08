package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func get(t *testing.T, h http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	signedIn(h, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzReportsOKAndVersion(t *testing.T) {
	h, _ := dbServerVersion(t, "abc123")
	rec := get(t, h, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q, want JSON", ct)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Version != "abc123" {
		t.Fatalf("body = %+v, want status ok and version abc123", body)
	}
}

func TestIndexShowsNameAndVersion(t *testing.T) {
	h, _ := dbServerVersion(t, "abc123")
	rec := get(t, h, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q, want HTML", ct)
	}
	for _, want := range []string{"<title>Intelligence</title>", "abc123"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("index misses %q", want)
		}
	}
}

func TestIndexHasNoGreeting(t *testing.T) {
	h, _ := dbServer(t)
	page := get(t, h, "/", map[string]string{"Tailscale-User-Name": "Some One"}).Body.String()
	if strings.Contains(page, "Hi") || strings.Contains(page, "Some One") {
		t.Errorf("the home page should open on the hosts, with no greeting")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h, _ := dbServer(t)
	if rec := get(t, h, "/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOnlyGETAndHEAD(t *testing.T) {
	h, _ := dbServer(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d, want 405", rec.Code)
	}
}

func TestIndexShowsShortVersionWithFullInTooltip(t *testing.T) {
	full := "8a578ee431111bbfa263522b95cae4a9c95ee5e2"
	h, _ := dbServerVersion(t, full)
	want := `<footer title="` + full + `">version 8a578ee43111</footer>`
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, want) {
		t.Errorf("index misses %s:\n%s", want, page)
	}
	if !strings.Contains(get(t, h, "/healthz", nil).Body.String(), full) {
		t.Errorf("/healthz must report the full version")
	}
}

func TestShortVersionLeavesShortValuesAlone(t *testing.T) {
	if got := short("dev"); got != "dev" {
		t.Fatalf("short(dev) = %q", got)
	}
}

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// dbServer is a server on its own test database, with an account signed in (testServer).
func dbServer(t *testing.T) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return newServer(t, d, d.Owner, "v"), d
}

func dbServerVersion(t *testing.T, v string) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return newServer(t, d, d.Owner, v), d
}

func hosts(t *testing.T, d dbtest.DB) []fleet.Host {
	t.Helper()
	b, err := fleet.New(d.Owner).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(t *testing.T, h http.Handler, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	signedIn(h, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var sampleReport = hostclient.Report{Name: "host-1", CollectedAt: t0, Version: "c1", Paseo: hostclient.PaseoStatus{
	Connected: true, Version: "0.15.3", Hostname: "host-1", PID: 3029, Listen: "0.0.0.0:6767",
	Providers: []hostclient.Provider{{Name: "claude", Available: true}, {Name: "copilot"}},
	Workspaces: []hostclient.Workspace{
		{ID: "w-app", Name: "Login fix", Kind: "worktree", Status: "running", Directory: "/w/app-login", Project: "example/app",
			Branch: "fix-login", Dirty: true, Additions: 12, Deletions: 3},
		{ID: "w-docs", Name: "Docs", Kind: "directory", Status: "done", Directory: "/w/docs", Project: "docs"},
		{ID: "w-idle", Name: "Quiet checkout", Kind: "local_checkout", Status: "done", Directory: "/w/quiet"},
	},
	Agents: []hostclient.Agent{
		{ID: "a1000000-0000-4000-8000-000000000001", WorkspaceID: "w-app", Title: "Fix the <b>login</b> page", Provider: "claude",
			Model: "claude-opus-5-5", Thinking: "medium", Status: "running", Cwd: "/w/app-login", CreatedAt: t0.Add(-time.Hour)},
		{ID: "a2000000-0000-4000-8000-000000000002", WorkspaceID: "w-docs", Title: "Write notes", Provider: "codex",
			Model: "gpt-6.1-sol", Thinking: "high", Status: "idle", Cwd: "/w/docs", CreatedAt: t0.Add(-2 * time.Hour)},
		{ID: "a3000000-0000-4000-8000-000000000003", WorkspaceID: "w-app", Title: "Audit", Provider: "claude",
			Model: "claude-opus-5-5", Thinking: "medium", Status: "idle", Cwd: "/w/app-login", CreatedAt: t0.Add(-3 * time.Hour),
			RequiresAttention: true, AttentionReason: "finished"},
	},
}}

func TestReportedHostIsListedInTheAPIAndOnTheHomePage(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)

	rec := get(t, h, "/api/hosts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/hosts = %d", rec.Code)
	}
	var list []fleet.Host
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Report.Name != "host-1" || !list[0].Online || len(list[0].Workspaces) != 3 ||
		len(list[0].Workspaces[0].Agents) != 2 {
		t.Fatalf("list = %+v", list)
	}

	page := get(t, h, "/", nil).Body.String()
	for _, want := range []string{"host-1", "online", "1 running, 2 idle", "Login fix", "example/app",
		"Fix the &lt;b&gt;login&lt;/b&gt; page", "claude-opus-5-5", "Write notes", "needs attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("home page misses %q", want)
		}
	}
}

func TestHomePageShowsSilentHostOffline(t *testing.T) {
	h, d := dbServer(t)
	keyOf(t, h, "host-1") // the account's
	if err := fleet.New(d.Owner).Record(t.Context(), sampleReport, time.Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, "offline") {
		t.Errorf("a host silent for 5 minutes should show offline")
	}
}

func TestHomePageSaysWhenNoHostHasReported(t *testing.T) {
	h, _ := dbServer(t)
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, "No hosts yet") {
		t.Errorf("empty fleet message missing")
	}
}

func TestHomePageFlagsAHostNotOnTheServersVersion(t *testing.T) {
	h, _ := dbServerVersion(t, "0123456789abcdef")
	rep := sampleReport
	rep.Version = "fedcba9876543210"
	sendReport(t, h, rep)
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, `title="fedcba9876543210">client fedcba987654 is not on the server's version`) {
		t.Errorf("a host on another version than the server should say so, with its version:\n%s", page)
	}
	rep.Version = "0123456789abcdef"
	sendReport(t, h, rep)
	if strings.Contains(get(t, h, "/", nil).Body.String(), "not on the server's version") {
		t.Errorf("a host on the server's version is not behind")
	}
}

func TestHomePageNestsAgentsInTheirWorkspaces(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	page := get(t, h, "/", nil).Body.String()
	app, run, audit, docs, notes := strings.Index(page, "Login fix"), strings.Index(page, "a100000"),
		strings.Index(page, "a300000"), strings.Index(page, "Docs"), strings.Index(page, "a200000")
	if !(app >= 0 && app < run && run < audit && audit < docs && docs < notes) {
		t.Fatalf("want Login fix (running a1, then a3) before Docs (a2): %d %d %d %d %d", app, run, audit, docs, notes)
	}
	if strings.Contains(page, "<b>login</b>") {
		t.Errorf("agent names must be escaped")
	}
}

func TestHomePageFoldsWorkspacesWithoutAgents(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	page := get(t, h, "/", nil).Body.String()
	fold, quiet := strings.Index(page, "1 workspace without agents"), strings.Index(page, "Quiet checkout")
	if fold < 0 || quiet < fold {
		t.Errorf("a workspace with no agents should be folded away under a count")
	}
}

func TestHomePageShowsADisconnectedHost(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, hostclient.Report{Name: "host-2", Paseo: hostclient.PaseoStatus{Error: "connect ws://127.0.0.1:6767/ws: refused"}})
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, "not connected") || !strings.Contains(page, "refused") {
		t.Errorf("a host that cannot reach its daemon should say so and why")
	}
	if !strings.Contains(page, "no agents") {
		t.Errorf("a host with no agents should say so")
	}
}

// details returns the <details> element with id in page, up to its first child's end, or "".
func detailsTag(page, id string) string {
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(page[:i], "<details")
	end := strings.Index(page[i:], ">")
	if start < 0 || end < 0 {
		return ""
	}
	return page[start : i+end+1]
}

func TestHomePageIsATreeOfHostsWorkspacesAndAgents(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	page := get(t, h, "/", nil).Body.String()

	host := detailsTag(page, "host-host-1")
	if host == "" || !strings.Contains(host, " open") {
		t.Fatalf("each host should be a details element, open by default: %q", host)
	}
	app, docs := detailsTag(page, "ws-host-1-w-app"), detailsTag(page, "ws-host-1-w-docs")
	if app == "" || docs == "" {
		t.Fatalf("each workspace with agents should be a details element under its host:\n%s", page)
	}
	if !strings.Contains(app, " open") {
		t.Errorf("a workspace with a running agent should start open: %q", app)
	}
	if strings.Contains(docs, " open") {
		t.Errorf("a workspace with only idle agents should start closed: %q", docs)
	}
	if !(strings.Index(page, `id="host-host-1"`) < strings.Index(page, `id="ws-host-1-w-app"`) &&
		strings.Index(page, `id="ws-host-1-w-app"`) < strings.Index(page, "a1000000-0000-4000-8000-000000000001")) {
		t.Errorf("the tree should nest host > workspace > agent")
	}
	for _, want := range []string{`class="status st-running">running`, `class="status st-idle">idle`} {
		if !strings.Contains(page, want) {
			t.Errorf("each agent should show its status: missing %q", want)
		}
	}
}

func TestHomePageRefreshesWithoutResettingTheTree(t *testing.T) {
	h, _ := dbServer(t)
	page := get(t, h, "/", nil).Body.String()
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Errorf("a full-page refresh would collapse what the reader opened")
	}
	if !strings.Contains(page, "<script>") || !strings.Contains(page, "setInterval") {
		t.Errorf("the page should refresh the tree in place")
	}
}
