package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/devbox"
)

const newAgentPath = "/devboxes/devbox-1/workspaces/w-app/agents/new"

// call makes one request with a JSON body (if any), extra headers and cookies.
func call(t *testing.T, h http.Handler, method, path, body string, header map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var sameOriginFetch = map[string]string{"Sec-Fetch-Site": "same-origin"}

// signInAs signs in with the operator token "op" and returns the session cookie.
func signInAs(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/session", `{"token":"op"}`, sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("sign in set no session cookie")
	return nil
}

func TestSessionAPISignsABrowserInWithTheOperatorToken(t *testing.T) {
	h, _ := cmdServer(t)
	for _, tok := range []string{"", "wrong", "secret"} { // the dev box token is not the operator's
		rec := call(t, h, http.MethodPost, "/api/session", `{"token":"`+tok+`"}`, sameOriginFetch)
		if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) > 0 {
			t.Errorf("token %q: %d, cookies %v", tok, rec.Code, rec.Result().Cookies())
		}
	}
	if rec := call(t, h, http.MethodPost, "/api/session", `{"token":"op"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site sign in: %d", rec.Code)
	}
	c := signInAs(t, h)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 || c.Path != "/" || strings.Contains(c.Value, "op") {
		t.Errorf("cookie = %+v: it holds a hash of the token, never the token", c)
	}
	out := call(t, h, http.MethodDelete, "/api/session", "", sameOriginFetch, c)
	if out.Code != http.StatusNoContent || len(out.Result().Cookies()) == 0 || out.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("sign out: %d %v", out.Code, out.Result().Cookies())
	}
	nobody, _ := dbServer(t) // no OPERATOR_TOKEN
	if rec := call(t, nobody, http.MethodPost, "/api/session", `{"token":""}`, sameOriginFetch); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no OPERATOR_TOKEN: %d, want 503", rec.Code)
	}
}

func TestTheCommandsAPITakesABrowserSession(t *testing.T) {
	h, _ := cmdServer(t)
	c := signInAs(t, h)
	path := "/api/devboxes/devbox-1/commands"
	if rec := call(t, h, http.MethodPost, path, archiveA1, sameOriginFetch, c); rec.Code != http.StatusAccepted {
		t.Errorf("same-origin with a session: %d %s", rec.Code, rec.Body)
	}
	// behind tailscale serve, the browser's origin need not be the Host this server sees
	if rec := call(t, h, http.MethodPost, path, archiveA1, map[string]string{"Sec-Fetch-Site": "same-origin",
		"Origin": "https://box.example"}, c); rec.Code != http.StatusAccepted {
		t.Errorf("same-origin behind a proxy: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPost, path, archiveA1, sameOriginFetch); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", rec.Code)
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "op"}
	if rec := call(t, h, http.MethodPost, path, archiveA1, sameOriginFetch, forged); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged session: %d", rec.Code)
	}
	for name, hdr := range map[string]map[string]string{
		"cross-site":                        {"Sec-Fetch-Site": "cross-site"},
		"same-site":                         {"Sec-Fetch-Site": "same-site"},
		"foreign origin":                    {"Origin": "https://evil.example"},
		"null origin":                       {"Origin": "null"},
		"cross-site with a matching origin": {"Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"},
	} {
		if rec := call(t, h, http.MethodPost, path, archiveA1, hdr, c); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
	// a form post (no JSON) is what another site could send without asking: refused
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(archiveA1)))
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain with a session: %d, want 415", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, path, archiveA1, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("neither token nor session: %d", rec.Code)
	}
}

func TestSpawnPagesNeedASession(t *testing.T) {
	h, _ := cmdServer(t)
	for _, p := range []string{newAgentPath, "/devboxes/devbox-1/workspaces/new"} {
		rec := get(t, h, p, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next="+url.QueryEscape(p) {
			t.Errorf("%s with no session: %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	login := get(t, h, "/login?next="+url.QueryEscape(newAgentPath), nil).Body.String()
	for _, want := range []string{`type="password"`, "/api/session", `"` + newAgentPath + `"`} {
		if !strings.Contains(login, want) {
			t.Errorf("login page misses %s", want)
		}
	}
	for _, next := range []string{"//evil.example/x", "https://evil.example/", "javascript:alert(1)"} {
		if page := get(t, h, "/login?next="+url.QueryEscape(next), nil).Body.String(); strings.Contains(page, "evil") ||
			strings.Contains(page, "javascript:") {
			t.Errorf("login page keeps the foreign next %q", next)
		}
	}
}

func TestNewAgentPage(t *testing.T) {
	h, _ := cmdServer(t)
	c := signInAs(t, h)
	rec := call(t, h, http.MethodGet, newAgentPath, "", nil, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("new agent page: %d %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{"Login fix", "/w/app-login", "fix-login", `<option value="claude" selected>`, `name="prompt"`,
		`value="claude-opus-5-5"`, `<option value="medium" selected>`, `value="bypassPermissions"`, `"/api/devboxes/devbox-1/commands"`,
		`"create_agent"`, `"w-app"`, "/api/sql"} {
		if !strings.Contains(page, want) {
			t.Errorf("new agent page misses %s", want)
		}
	}
	if strings.Contains(page, `<option value="copilot"`) {
		t.Errorf("an unavailable provider is offered")
	}
	if rec := call(t, h, http.MethodGet, "/devboxes/devbox-1/workspaces/w-nope/agents/new", "", nil, c); rec.Code != http.StatusNotFound {
		t.Errorf("unknown workspace: %d", rec.Code)
	}
}

func TestNewWorkspacePageOffersTheBoxsRepositories(t *testing.T) {
	h, _ := cmdServer(t)
	rep := sampleReport
	rep.Paseo.Workspaces = append([]devbox.Workspace{}, sampleReport.Paseo.Workspaces...)
	rep.Paseo.Workspaces[0].ProjectRoot = "/w/app"
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, devbox.Workspace{ID: "w-app2", Name: "Other", Directory: "/w/app-other",
		Project: "example/app", ProjectRoot: "/w/app"})
	sendReport(t, h, rep)
	c := signInAs(t, h)
	rec := call(t, h, http.MethodGet, "/devboxes/devbox-1/workspaces/new", "", nil, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("new workspace page: %d %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{`<option value="/w/app">example/app · /w/app</option>`, `name="worktree" checked`, `name="branch"`,
		`name="base"`, `name="prompt"`, `"create_workspace"`, `"/api/devboxes/devbox-1/commands"`} {
		if !strings.Contains(page, want) {
			t.Errorf("new workspace page misses %s", want)
		}
	}
	if strings.Count(page, `<option value="/w/app">`) != 1 {
		t.Errorf("a repository is offered once, however many workspaces it has")
	}
	if rec := call(t, h, http.MethodGet, "/devboxes/nobox/workspaces/new", "", nil, c); rec.Code != http.StatusNotFound {
		t.Errorf("unknown box: %d", rec.Code)
	}
}

func TestHomePageLinksToSpawning(t *testing.T) {
	h, _ := cmdServer(t)
	home := get(t, h, "/", nil).Body.String()
	for _, want := range []string{`href="` + newAgentPath + `"`, `href="/devboxes/devbox-1/workspaces/w-idle/agents/new"`,
		`href="/devboxes/devbox-1/workspaces/new"`, `href="/login"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home page misses %s", want)
		}
	}
	c := signInAs(t, h)
	if signedIn := call(t, h, http.MethodGet, "/", "", nil, c).Body.String(); !strings.Contains(signedIn, `id="sign-out"`) {
		t.Errorf("a signed-in home page offers to sign out")
	}
}

// The spawn pages follow a command through the read-only SQL API with commandProgressSQL.
func TestCommandProgressIsReadableThroughSQL(t *testing.T) {
	h, _ := cmdServer(t)
	b := openStream(t, h, "devbox-1")
	_, cmd, _ := enqueue(t, h, "devbox-1", "op", `{"kind":"create_workspace","args":{"cwd":"/w/app","worktree":true,
		"agent":{"provider":"claude","prompt":"go"}}}`)
	progress := func() []any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"query": strings.ReplaceAll(commandProgressSQL, "$ID", cmd.ID)})
		code, res := sqlPost(t, h, string(body))
		if code != http.StatusOK || len(res.Rows) != 1 {
			t.Fatalf("progress: %d %+v", code, res)
		}
		return res.Rows[0]
	}
	if got := b.command(); got.ID != cmd.ID { // sending is asynchronous: wait until the box has it
		t.Fatalf("box got %+v", got)
	}
	if row := progress(); row[0] != "sent" || row[4] != false {
		t.Errorf("sent: %v", row)
	}
	b.call(devbox.Frame{Type: devbox.CommandUpdateFrame, Update: &devbox.CommandUpdate{ID: cmd.ID, Status: devbox.CommandSucceeded,
		Result: json.RawMessage(`{"workspaceId":"w-new","agentId":"a9"}`)}})
	if row := progress(); row[0] != "succeeded" || row[2] != "w-new" || row[3] != "a9" || row[4] != false {
		t.Errorf("succeeded, not yet reported: %v", row)
	}
	rep := sampleReport
	rep.Paseo.Workspaces = append(append([]devbox.Workspace{}, sampleReport.Paseo.Workspaces...), devbox.Workspace{ID: "w-new"})
	rep.Paseo.Agents = append(append([]devbox.Agent{}, sampleReport.Paseo.Agents...), devbox.Agent{ID: "a9", WorkspaceID: "w-new"})
	b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &rep})
	if row := progress(); row[4] != true {
		t.Errorf("reported: %v", row)
	}
}
