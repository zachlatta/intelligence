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
	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

func get(t *testing.T, h http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
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
		t.Errorf("the home page should open on the dev boxes, with no greeting")
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

// dbServer is a server on its own test database, with the dev box token "secret".
func dbServer(t *testing.T) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return New(Config{Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret"}), d
}

func dbServerVersion(t *testing.T, v string) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return New(Config{Version: v, DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret"}), d
}

func boxes(t *testing.T, d dbtest.DB) []fleet.Box {
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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func reportJSON(t *testing.T, r devbox.Report) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var sampleReport = devbox.Report{Name: "devbox-1", CollectedAt: t0, Version: "c1", Paseo: devbox.PaseoStatus{
	Connected: true, Version: "0.15.3", Hostname: "devbox-1", PID: 3029, Listen: "0.0.0.0:6767",
	Providers: []devbox.Provider{{Name: "claude", Available: true}, {Name: "copilot"}},
	Workspaces: []devbox.Workspace{
		{ID: "w-app", Name: "Login fix", Kind: "worktree", Status: "running", Directory: "/w/app-login", Project: "example/app",
			Branch: "fix-login", Dirty: true, Additions: 12, Deletions: 3},
		{ID: "w-docs", Name: "Docs", Kind: "directory", Status: "done", Directory: "/w/docs", Project: "docs"},
		{ID: "w-idle", Name: "Quiet checkout", Kind: "local_checkout", Status: "done", Directory: "/w/quiet"},
	},
	Agents: []devbox.Agent{
		{ID: "a1000000-0000-4000-8000-000000000001", WorkspaceID: "w-app", Title: "Fix the <b>login</b> page", Provider: "claude",
			Model: "claude-opus-5-5", Thinking: "medium", Status: "running", Cwd: "/w/app-login", CreatedAt: t0.Add(-time.Hour)},
		{ID: "a2000000-0000-4000-8000-000000000002", WorkspaceID: "w-docs", Title: "Write notes", Provider: "codex",
			Model: "gpt-6.1-sol", Thinking: "high", Status: "idle", Cwd: "/w/docs", CreatedAt: t0.Add(-2 * time.Hour)},
		{ID: "a3000000-0000-4000-8000-000000000003", WorkspaceID: "w-app", Title: "Audit", Provider: "claude",
			Model: "claude-opus-5-5", Thinking: "medium", Status: "idle", Cwd: "/w/app-login", CreatedAt: t0.Add(-3 * time.Hour),
			RequiresAttention: true, AttentionReason: "finished"},
	},
}}

func TestReportNeedsTheToken(t *testing.T) {
	h, d := dbServer(t)
	body := reportJSON(t, sampleReport)
	for _, tok := range []string{"", "wrong"} {
		if rec := post(t, h, devbox.ReportPath, tok, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", tok, rec.Code)
		}
	}
	if len(boxes(t, d)) != 0 {
		t.Fatalf("an unauthorized report was recorded")
	}
}

func TestReportsRefusedWhenNoTokenIsConfigured(t *testing.T) {
	d := dbtest.New(t)
	h := New(Config{Version: "v", DB: d.Owner, Readonly: d.Readonly})
	if rec := post(t, h, devbox.ReportPath, "", reportJSON(t, sampleReport)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if len(boxes(t, d)) != 0 {
		t.Fatalf("report recorded with no token configured")
	}
}

func TestReportRejectsBadNamesAndBodies(t *testing.T) {
	h, _ := dbServer(t)
	bad := sampleReport
	bad.Name = "<script>"
	if rec := post(t, h, devbox.ReportPath, "secret", reportJSON(t, bad)); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, devbox.ReportPath, "secret", []byte("{not json")); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: status = %d, want 400", rec.Code)
	}
	huge := append([]byte(`{"name":"x","pad":"`), bytes.Repeat([]byte("a"), 2<<20)...)
	huge = append(huge, []byte(`"}`)...)
	if rec := post(t, h, devbox.ReportPath, "secret", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: status = %d, want 413", rec.Code)
	}
}

func TestReportedBoxIsListedInTheAPIAndOnTheHomePage(t *testing.T) {
	h, _ := dbServer(t)
	if rec := post(t, h, devbox.ReportPath, "secret", reportJSON(t, sampleReport)); rec.Code != http.StatusNoContent {
		t.Fatalf("report status = %d (%s), want 204", rec.Code, rec.Body)
	}

	rec := get(t, h, "/api/devboxes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/devboxes = %d", rec.Code)
	}
	var list []fleet.Box
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Report.Name != "devbox-1" || !list[0].Online || len(list[0].Workspaces) != 3 ||
		len(list[0].Workspaces[0].Agents) != 2 {
		t.Fatalf("list = %+v", list)
	}

	page := get(t, h, "/", nil).Body.String()
	for _, want := range []string{"devbox-1", "online", "connected", "0.15.3", "claude", "1 running, 2 idle", "3 workspaces",
		"Login fix", "worktree", "fix-login", "+12", "−3", "example/app", "Fix the &lt;b&gt;login&lt;/b&gt; page", "a100000",
		"claude-opus-5-5", "Write notes", "needs attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("home page misses %q", want)
		}
	}
}

func TestHomePageShowsSilentBoxOffline(t *testing.T) {
	h, d := dbServer(t)
	if err := fleet.New(d.Owner).Record(t.Context(), sampleReport, time.Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, "offline") {
		t.Errorf("a box silent for 5 minutes should show offline")
	}
}

func TestHomePageSaysWhenNoBoxHasReported(t *testing.T) {
	h, _ := dbServer(t)
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, "No dev boxes have reported yet") {
		t.Errorf("empty fleet message missing")
	}
}

func TestHomePageShowsEachBoxClientVersion(t *testing.T) {
	h, _ := dbServerVersion(t, "0123456789abcdef")
	rep := sampleReport
	rep.Version = "fedcba9876543210"
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, rep))
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, `title="fedcba9876543210">fedcba987654</span>`) {
		t.Errorf("box's client version missing:\n%s", page)
	}
	if !strings.Contains(page, "not on the server's version") {
		t.Errorf("a box on another version than the server should say so")
	}
	rep.Version = "0123456789abcdef"
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, rep))
	if strings.Contains(get(t, h, "/", nil).Body.String(), "not on the server's version") {
		t.Errorf("a box on the server's version is not behind")
	}
}

func TestHomePageNestsAgentsInTheirWorkspaces(t *testing.T) {
	h, _ := dbServer(t)
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, sampleReport))
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
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, sampleReport))
	page := get(t, h, "/", nil).Body.String()
	fold, quiet := strings.Index(page, "1 workspace without agents"), strings.Index(page, "Quiet checkout")
	if fold < 0 || quiet < fold {
		t.Errorf("a workspace with no agents should be folded away under a count")
	}
}

func TestHomePageShowsADisconnectedBox(t *testing.T) {
	h, _ := dbServer(t)
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, devbox.Report{Name: "devbox-2",
		Paseo: devbox.PaseoStatus{Error: "connect ws://127.0.0.1:6767/ws: refused"}}))
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, "not connected") || !strings.Contains(page, "refused") {
		t.Errorf("a box that cannot reach its daemon should say so and why")
	}
}
