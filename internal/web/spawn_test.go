package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

const newAgentPath = "/hosts/host-1/workspaces/w-app/agents/new"

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
	signedIn(h, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var sameOriginFetch = map[string]string{"Sec-Fetch-Site": "same-origin"}

// signInAs signs the test account in through the session API and returns its cookie.
func signInAs(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := call(t, anon(h), http.MethodPost, "/api/session", `{"username":"someone","password":"`+testPassword+`"}`, sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body)
	}
	return cookieOf(t, rec)
}

func cookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie: %d %s", rec.Code, rec.Body)
	return nil
}

func TestSignUpSignInAndSignOut(t *testing.T) {
	h, _ := cmdServer(t)
	h = anon(h)
	up := call(t, h, http.MethodPost, "/api/accounts", `{"username":"Ann","password":"ann's long password"}`, sameOriginFetch)
	if up.Code != http.StatusCreated || !strings.Contains(up.Body.String(), `"username":"ann"`) {
		t.Fatalf("sign up: %d %s", up.Code, up.Body)
	}
	c := cookieOf(t, up)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 || c.Path != "/" || strings.Contains(c.Value, "password") {
		t.Errorf("cookie = %+v", c)
	}
	if page := call(t, h, http.MethodGet, "/", "", nil, c); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "ann") {
		t.Errorf("home page after sign up: %d", page.Code)
	}
	for body, want := range map[string]int{
		`{"username":"ann","password":"ann's long password"}`:     http.StatusConflict,
		`{"username":"bob","password":"short"}`:                   http.StatusBadRequest,
		`{"username":"no spaces","password":"a long enough one"}`: http.StatusBadRequest,
		`{"username":"bob"}`:                                      http.StatusBadRequest,
		`{"token":"anything"}`:                                    http.StatusBadRequest,
	} {
		if rec := call(t, h, http.MethodPost, "/api/accounts", body, sameOriginFetch); rec.Code != want || len(rec.Result().Cookies()) > 0 {
			t.Errorf("sign up %s: %d, want %d", body, rec.Code, want)
		}
	}
	cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
	if rec := call(t, h, http.MethodPost, "/api/accounts", `{"username":"eve","password":"eve's long password"}`, cross); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site sign up: %d", rec.Code)
	}
	for _, body := range []string{`{"username":"ann","password":"not ann's password"}`, `{"username":"nobody","password":"ann's long password"}`} {
		if rec := call(t, h, http.MethodPost, "/api/session", body, sameOriginFetch); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) > 0 {
			t.Errorf("sign in %s: %d", body, rec.Code)
		}
	}
	if rec := call(t, h, http.MethodPost, "/api/session", `{"username":"ann","password":"ann's long password"}`, cross); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site sign in: %d", rec.Code)
	}
	in := call(t, h, http.MethodPost, "/api/session", `{"username":"ANN","password":"ann's long password"}`, sameOriginFetch)
	c2 := cookieOf(t, in)
	out := call(t, h, http.MethodDelete, "/api/session", "", sameOriginFetch, c2)
	if out.Code != http.StatusNoContent || out.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("sign out: %d %v", out.Code, out.Result().Cookies())
	}
	if rec := call(t, h, http.MethodGet, "/", "", nil, c2); rec.Code != http.StatusSeeOther {
		t.Errorf("a signed-out cookie still reads: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodGet, "/", "", nil, c); rec.Code != http.StatusOK {
		t.Errorf("signing one browser out signed out another: %d", rec.Code)
	}
}

func TestChangingThePasswordSignsOutOtherBrowsers(t *testing.T) {
	h, _ := cmdServer(t)
	other := signInAs(t, h)
	this := signInAs(t, h)
	path := "/api/account/password"
	if rec := call(t, anon(h), http.MethodPut, path, `{"current":"wrong","password":"a brand new password"}`, sameOriginFetch, this); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong current password: %d", rec.Code)
	}
	rec := call(t, anon(h), http.MethodPut, path, `{"current":"`+testPassword+`","password":"a brand new password"}`, sameOriginFetch, this)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if got := call(t, anon(h), http.MethodGet, "/", "", nil, cookieOf(t, rec)); got.Code != http.StatusOK {
		t.Errorf("this browser's new session: %d", got.Code)
	}
	if got := call(t, anon(h), http.MethodGet, "/", "", nil, other); got.Code != http.StatusSeeOther {
		t.Errorf("another browser stayed signed in: %d", got.Code)
	}
}

func TestTheCommandsAPITakesABrowserSession(t *testing.T) {
	h, _ := cmdServer(t)
	c := signInAs(t, h)
	path := "/api/hosts/host-1/commands"
	if rec := call(t, h, http.MethodPost, path, archiveA1, sameOriginFetch, c); rec.Code != http.StatusAccepted {
		t.Errorf("same-origin with a session: %d %s", rec.Code, rec.Body)
	}
	// behind tailscale serve, the browser's origin need not be the Host this server sees
	if rec := call(t, h, http.MethodPost, path, archiveA1, map[string]string{"Sec-Fetch-Site": "same-origin",
		"Origin": "https://host.example"}, c); rec.Code != http.StatusAccepted {
		t.Errorf("same-origin behind a proxy: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, anon(h), http.MethodPost, path, archiveA1, sameOriginFetch); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", rec.Code)
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "made-up"}
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
	if rec := call(t, anon(h), http.MethodPost, path, archiveA1, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")}); rec.Code != http.StatusUnauthorized {
		t.Errorf("a host's key is no session: %d", rec.Code)
	}
}

func TestSpawnPagesNeedASession(t *testing.T) {
	h, _ := cmdServer(t)
	for _, p := range []string{newAgentPath, "/hosts/host-1/workspaces/new"} {
		rec := get(t, anon(h), p, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next="+url.QueryEscape(p) {
			t.Errorf("%s with no session: %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	login := get(t, h, "/login?next="+url.QueryEscape(newAgentPath), nil).Body.String()
	for _, want := range []string{`type="password"`, "/api/session", "/api/accounts", `"` + newAgentPath + `"`} {
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
		`value="claude-opus-5-5"`, `<option value="medium" selected>`, `value="bypassPermissions"`, `"/api/hosts/host-1/commands"`,
		`"create_agent"`, `"w-app"`, "/api/sql"} {
		if !strings.Contains(page, want) {
			t.Errorf("new agent page misses %s", want)
		}
	}
	if strings.Contains(page, `<option value="copilot"`) {
		t.Errorf("an unavailable provider is offered")
	}
	if rec := call(t, h, http.MethodGet, "/hosts/host-1/workspaces/w-nope/agents/new", "", nil, c); rec.Code != http.StatusNotFound {
		t.Errorf("unknown workspace: %d", rec.Code)
	}
}

func TestNewWorkspacePageOffersTheHostsRepositories(t *testing.T) {
	h, _ := cmdServer(t)
	rep := sampleReport
	rep.Paseo.Workspaces = append([]hostclient.Workspace{}, sampleReport.Paseo.Workspaces...)
	rep.Paseo.Workspaces[0].ProjectRoot = "/w/app"
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, hostclient.Workspace{ID: "w-app2", Name: "Other", Directory: "/w/app-other",
		Project: "example/app", ProjectRoot: "/w/app"})
	sendReport(t, h, rep)
	c := signInAs(t, h)
	rec := call(t, h, http.MethodGet, "/hosts/host-1/workspaces/new", "", nil, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("new workspace page: %d %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{`<option value="/w/app">example/app · /w/app</option>`, `name="worktree" checked`, `name="branch"`,
		`name="base"`, `name="prompt"`, `"create_workspace"`, `"/api/hosts/host-1/commands"`} {
		if !strings.Contains(page, want) {
			t.Errorf("new workspace page misses %s", want)
		}
	}
	if strings.Count(page, `<option value="/w/app">`) != 1 {
		t.Errorf("a repository is offered once, however many workspaces it has")
	}
	if rec := call(t, h, http.MethodGet, "/hosts/nohost/workspaces/new", "", nil, c); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: %d", rec.Code)
	}
}

func TestHomePageLinksToSpawning(t *testing.T) {
	h, _ := cmdServer(t)
	home := get(t, h, "/", nil).Body.String()
	for _, want := range []string{`href="` + newAgentPath + `"`, `href="/hosts/host-1/workspaces/w-idle/agents/new"`,
		`href="/hosts/host-1/workspaces/new"`, `href="/account#add"`} {
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
	b := openStream(t, h, "host-1")
	_, cmd, _ := enqueue(t, h, "host-1", "", `{"kind":"create_workspace","args":{"cwd":"/w/app","worktree":true,
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
	if got := b.command(); got.ID != cmd.ID { // sending is asynchronous: wait until the host has it
		t.Fatalf("host got %+v", got)
	}
	if row := progress(); row[0] != "sent" || row[4] != false {
		t.Errorf("sent: %v", row)
	}
	b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandSucceeded,
		Result: json.RawMessage(`{"workspaceId":"w-new","agentId":"a9"}`)}})
	if row := progress(); row[0] != "succeeded" || row[2] != "w-new" || row[3] != "a9" || row[4] != false {
		t.Errorf("succeeded, not yet reported: %v", row)
	}
	rep := sampleReport
	rep.Paseo.Workspaces = append(append([]hostclient.Workspace{}, sampleReport.Paseo.Workspaces...), hostclient.Workspace{ID: "w-new"})
	rep.Paseo.Agents = append(append([]hostclient.Agent{}, sampleReport.Paseo.Agents...), hostclient.Agent{ID: "a9", WorkspaceID: "w-new"})
	b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep})
	if row := progress(); row[4] != true {
		t.Errorf("reported: %v", row)
	}
}

// pageCatalog is the provider catalog a page carries for its model, thinking and mode pickers.
func pageCatalog(t *testing.T, page string) map[string]hostclient.Provider {
	t.Helper()
	const open = `<script id="provider-catalog" type="application/json">`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("the page carries no provider catalog")
	}
	rest := page[i+len(open):]
	var out map[string]hostclient.Provider
	if err := json.Unmarshal([]byte(rest[:strings.Index(rest, "</script>")]), &out); err != nil {
		t.Fatalf("provider catalog: %v", err)
	}
	return out
}

// TestSpawnFormsKnowEachProvidersModels: the forms that start an agent carry the models, thinking
// levels and modes of each provider the host can run, so they offer them as choices.
func TestSpawnFormsKnowEachProvidersModels(t *testing.T) {
	h, _ := cmdServer(t)
	rep := sampleReport
	rep.Paseo.Providers = []hostclient.Provider{{Name: "claude", Available: true, Label: "Claude", DefaultMode: "auto",
		Models: []hostclient.Model{{ID: "claude-opus-5-5", Label: "Opus 5.5", Default: true, Thinking: []string{"low", "medium"}, DefaultThinking: "medium"},
			{ID: "claude-sonnet-5-5", Label: "Sonnet 5.5"}},
		Modes: []hostclient.Mode{{ID: "auto", Label: "Auto mode"}, {ID: "bypassPermissions", Label: "Bypass"}}},
		{Name: "codex", Available: true}, {Name: "copilot", Models: []hostclient.Model{{ID: "gpt"}}},
		{Name: "antigravity", Available: true, DefaultMode: "full-access",
			Models: []hostclient.Model{{ID: "gemini-4-carbon-high", Label: "Gemini 4 Carbon (High)"},
				{ID: "gemini-4-carbon-medium"}, {ID: "gemini-4-carbon-low"}, {ID: "gemini-4-argon-high"}},
			Modes: []hostclient.Mode{{ID: "full-access"}}}}
	sendReport(t, h, rep)
	sendSchedules(t, h, sampleSchedules)
	c := signInAs(t, h)
	for _, path := range []string{"/hosts/host-1/workspaces/new", newAgentPath, "/hosts/host-1/schedules/new", "/hosts/host-1/schedules/sch-nightly"} {
		rec := call(t, h, http.MethodGet, path, "", nil, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		page := rec.Body.String()
		cat := pageCatalog(t, page)
		if cl := cat["claude"]; len(cl.Models) != 2 || cl.Models[0].Label != "Opus 5.5" || !cl.Models[0].Default ||
			cl.Models[0].DefaultThinking != "medium" || len(cl.Modes) != 2 || cl.DefaultMode != "auto" {
			t.Errorf("%s: claude = %+v", path, cl)
		}
		if _, ok := cat["codex"]; !ok {
			t.Errorf("%s: a provider with no catalog yet is left out: %v", path, cat)
		}
		if ag := cat["antigravity"]; len(ag.Models) != 4 || ag.Models[0].ID != "gemini-4-carbon-high" ||
			ag.Models[0].Label != "Gemini 4 Carbon (High)" || ag.Models[1].ID != "gemini-4-carbon-medium" ||
			ag.Models[2].ID != "gemini-4-carbon-low" || ag.Models[3].ID != "gemini-4-argon-high" || ag.DefaultMode != "full-access" {
			t.Errorf("%s: antigravity = %+v", path, ag)
		}
		if _, ok := cat["copilot"]; ok {
			t.Errorf("%s: an unavailable provider's catalog is offered", path)
		}
		if !strings.Contains(page, "providerPickers(") {
			t.Errorf("%s: the form does not turn its fields into pickers", path)
		}
	}
}
