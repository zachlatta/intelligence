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

func TestIndexGreetsTailnetUser(t *testing.T) {
	h, _ := dbServer(t)
	rec := get(t, h, "/", map[string]string{"Tailscale-User-Login": "someone@example.com", "Tailscale-User-Name": "Some One"})
	if !strings.Contains(rec.Body.String(), "Some One") {
		t.Errorf("index does not greet the tailnet user: %s", rec.Body.String())
	}
}

func TestIndexEscapesTailnetUser(t *testing.T) {
	h, _ := dbServer(t)
	rec := get(t, h, "/", map[string]string{"Tailscale-User-Name": "<script>x</script>"})
	if strings.Contains(rec.Body.String(), "<script>x</script>") {
		t.Errorf("user name is not escaped")
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
	Daemon: "running", Connected: "reachable", Version: "0.11.4", Hostname: "devbox-1", PID: 3029,
	Providers: []devbox.Provider{{Name: "claude", Status: "available", Available: true}, {Name: "copilot", Status: "unavailable"}},
	Agents: []devbox.Agent{
		{ID: "a1", ShortID: "a1short", Name: "Fix the <b>login</b> page", Provider: "claude/claude-opus-5-5", Thinking: "medium", Status: "running", Cwd: "~/dev/example/app"},
		{ID: "a2", ShortID: "a2short", Name: "Write notes", Provider: "codex/gpt-6.1-sol", Thinking: "high", Status: "idle", Cwd: "~/dev/example/docs"},
		{ID: "a3", ShortID: "a3short", Name: "Audit", Provider: "claude/claude-opus-5-5", Thinking: "medium", Status: "idle", Cwd: "~/dev/example/app"},
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
	if len(list) != 1 || list[0].Report.Name != "devbox-1" || !list[0].Online || len(list[0].Report.Paseo.Agents) != 3 {
		t.Fatalf("list = %+v", list)
	}

	page := get(t, h, "/", nil).Body.String()
	for _, want := range []string{"devbox-1", "online", "running", "reachable", "0.11.4", "claude", "1 running, 2 idle",
		"Fix the &lt;b&gt;login&lt;/b&gt; page", "a1short", "claude/claude-opus-5-5", "~/dev/example/docs", "Write notes"} {
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

func TestHomePageListsAgentsRunningFirst(t *testing.T) {
	h, _ := dbServer(t)
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, sampleReport))
	page := get(t, h, "/", nil).Body.String()
	run, idle := strings.Index(page, "a1short"), strings.Index(page, "a3short")
	if run < 0 || idle < 0 || run > idle {
		t.Fatalf("running agent a1short should come before idle a3short")
	}
	if strings.Contains(page, "<b>login</b>") {
		t.Errorf("agent names must be escaped")
	}
}

func TestHomePageSaysWhenABoxHasNoAgents(t *testing.T) {
	h, _ := dbServer(t)
	rep := sampleReport
	rep.Paseo.Agents = nil
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, rep))
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, "No agents") {
		t.Errorf("a box with no agents should say so")
	}
}
