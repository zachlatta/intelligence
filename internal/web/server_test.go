package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	h := New(Config{Version: "abc123"})
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
	h := New(Config{Version: "abc123"})
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
	h := New(Config{Version: "v"})
	rec := get(t, h, "/", map[string]string{"Tailscale-User-Login": "someone@example.com", "Tailscale-User-Name": "Some One"})
	if !strings.Contains(rec.Body.String(), "Some One") {
		t.Errorf("index does not greet the tailnet user: %s", rec.Body.String())
	}
}

func TestIndexEscapesTailnetUser(t *testing.T) {
	h := New(Config{Version: "v"})
	rec := get(t, h, "/", map[string]string{"Tailscale-User-Name": "<script>x</script>"})
	if strings.Contains(rec.Body.String(), "<script>x</script>") {
		t.Errorf("user name is not escaped")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h := New(Config{Version: "v"})
	if rec := get(t, h, "/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOnlyGETAndHEAD(t *testing.T) {
	h := New(Config{Version: "v"})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d, want 405", rec.Code)
	}
}

func TestIndexShowsShortVersionWithFullInTooltip(t *testing.T) {
	full := "8a578ee431111bbfa263522b95cae4a9c95ee5e2"
	h := New(Config{Version: full})
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

// fleetServer is a server with a dev-box registry on a settable clock and the token "secret".
func fleetServer(now *time.Time) (http.Handler, *fleet.Registry) {
	reg := fleet.New(func() time.Time { return *now }, time.Minute)
	return New(Config{Version: "v", Fleet: reg, DevboxToken: "secret"}), reg
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

var mewReport = devbox.Report{Name: "mew-dev", CollectedAt: t0, Version: "c1", Paseo: devbox.PaseoStatus{
	Daemon: "running", Connected: "reachable", Version: "0.11.4", Hostname: "mew-dev", PID: 3029,
	Providers: []devbox.Provider{{Name: "claude", Status: "available", Available: true}, {Name: "copilot", Status: "unavailable"}},
	Agents:    map[string]int{"running": 2, "idle": 8},
}}

func TestReportNeedsTheToken(t *testing.T) {
	now := t0
	h, reg := fleetServer(&now)
	body := reportJSON(t, mewReport)
	for _, tok := range []string{"", "wrong"} {
		if rec := post(t, h, devbox.ReportPath, tok, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", tok, rec.Code)
		}
	}
	if len(reg.List()) != 0 {
		t.Fatalf("an unauthorized report was recorded")
	}
}

func TestReportsRefusedWhenNoTokenIsConfigured(t *testing.T) {
	reg := fleet.New(time.Now, time.Minute)
	h := New(Config{Version: "v", Fleet: reg})
	if rec := post(t, h, devbox.ReportPath, "", reportJSON(t, mewReport)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if len(reg.List()) != 0 {
		t.Fatalf("report recorded with no token configured")
	}
}

func TestReportRejectsBadNamesAndBodies(t *testing.T) {
	now := t0
	h, _ := fleetServer(&now)
	bad := mewReport
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
	now := t0
	h, _ := fleetServer(&now)
	if rec := post(t, h, devbox.ReportPath, "secret", reportJSON(t, mewReport)); rec.Code != http.StatusNoContent {
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
	if len(list) != 1 || list[0].Report.Name != "mew-dev" || !list[0].Online || list[0].Report.Paseo.Agents["running"] != 2 {
		t.Fatalf("list = %+v", list)
	}

	page := get(t, h, "/", nil).Body.String()
	for _, want := range []string{"mew-dev", "online", "running", "reachable", "0.11.4", "claude", "2 running", "8 idle"} {
		if !strings.Contains(page, want) {
			t.Errorf("home page misses %q", want)
		}
	}
}

func TestHomePageShowsSilentBoxOffline(t *testing.T) {
	now := t0
	h, _ := fleetServer(&now)
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, mewReport))
	now = t0.Add(5 * time.Minute)
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, "offline") {
		t.Errorf("a box silent for 5 minutes should show offline")
	}
}

func TestHomePageSaysWhenNoBoxHasReported(t *testing.T) {
	now := t0
	h, _ := fleetServer(&now)
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, "No dev boxes have reported yet") {
		t.Errorf("empty fleet message missing")
	}
}
