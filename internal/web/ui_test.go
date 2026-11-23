package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestTheStylesheetIsServed(t *testing.T) {
	h, _ := dbServer(t)
	rec := get(t, h, "/ui.css", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("ui.css: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{"--accent", "--topbar-h", ".topnav", ".card", ".badge", ".btn", ".md table", ".progress.warn", "prefers-color-scheme: dark", ".btn[hidden] { display: none; }", ".fields[hidden] { display: none; }"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("ui.css misses %q", want)
		}
	}
}

// The shared assets are revalidated on each load, and a browser's copy that is current is not sent again.
func TestTheSharedAssetsAreRevalidated(t *testing.T) {
	h, _ := dbServer(t)
	for _, path := range []string{"/ui.css", "/dictation.js", "/submitkeys.js"} {
		rec := get(t, h, path, nil)
		tag := rec.Header().Get("ETag")
		if rec.Code != http.StatusOK || tag == "" || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d, etag %q, cache-control %q", path, rec.Code, tag, rec.Header().Get("Cache-Control"))
			continue
		}
		if again := get(t, h, path, map[string]string{"If-None-Match": tag}); again.Code != http.StatusNotModified {
			t.Errorf("%s with its own etag: %d, want 304", path, again.Code)
		}
	}
}

// Every page shares one stylesheet and one top bar, with its own tab marked.
func TestEveryPageSharesTheStylesheetAndTopBar(t *testing.T) {
	h, _ := cmdServer(t)
	c := signInAs(t, h)
	sendBatch(t, h, batch(true, "ep1", tentry(1, `{"type":"user_message","text":"go"}`)))
	pages := []struct{ path, tab string }{
		{"/", `href="/" aria-current="page"`},
		{"/workflows", `href="/workflows" aria-current="page"`},
		{"/workflows/email", `href="/workflows" aria-current="page"`},
		{"/schedules", `href="/schedules" aria-current="page"`},
		{"/usage", `href="/usage" aria-current="page"`},
		{"/resources", `href="/resources" aria-current="page"`},
		{"/account", `href="/account" aria-current="page"`},
		{"/workflow-proposals", `href="/workflow-proposals" id="proposals-nav" aria-current="page"`},
		{"/whats-new", `href="/whats-new" id="nav-whats-new" aria-current="page"`},
		{newAgentPath, `href="/" aria-current="page"`},
		{"/hosts/host-1/workspaces/new", `href="/" aria-current="page"`},
		{"/hosts/host-1/agents/" + agentA1, `href="/" aria-current="page"`},
		{"/hosts/host-1/agents/" + agentA1 + "/entries/1", `href="/" aria-current="page"`},
	}
	for _, p := range pages {
		rec := call(t, h, http.MethodGet, p.path, "", nil, c)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p.path, rec.Code)
			continue
		}
		page := rec.Body.String()
		nav, _, _ := strings.Cut(page[strings.Index(page, `class="topnav"`)+1:], "</nav>")
		for _, tab := range []string{">Hosts<", ">Workflows<", ">Proposals<", ">Schedules<", ">Plans<", ">Host monitoring<", ">What's New<", ">Account<"} { // one tab each
			if n := strings.Count(nav, tab); n != 1 {
				t.Errorf("%s: %d tabs %s, want 1", p.path, n, tab)
			}
		}
		for _, want := range []string{`<link rel="stylesheet" href="/ui.css`, `class="topnav"`, p.tab} {
			if !strings.Contains(page, want) {
				t.Errorf("%s misses %q", p.path, want)
			}
		}
		if strings.Count(nav, `aria-current="page"`) != 1 {
			t.Errorf("%s should mark exactly one tab", p.path)
		}
		if n := strings.Count(page, "<body"); n != 1 {
			t.Errorf("%s: %d <body> tags, want 1", p.path, n)
		}
	}
}

// The sign-in page keeps the top bar's brand but shows no tabs: a signed-out visitor cannot open them.
func TestLoginPageHasNoTabs(t *testing.T) {
	h, _ := cmdServer(t)
	rec := get(t, h, "/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{`class="topbar"`, `class="brand"`, `<link rel="stylesheet" href="/ui.css`} {
		if !strings.Contains(page, want) {
			t.Errorf("login page misses %q", want)
		}
	}
	for _, bad := range []string{`class="topnav"`, `aria-current="page"`, ">Host monitoring<", `id="nav-whats-new"`,
		`id="proposals-count"`, `workflow_proposal_status`} {
		if strings.Contains(page, bad) {
			t.Errorf("login page has %q", bad)
		}
	}
}

func checkNoRenamedCSSProperties(sources map[string]string) []string {
	var errs []string
	for name, src := range sources {
		for _, bad := range []string{"host-shadow", "host-sizing", "-webkit-host"} {
			if strings.Contains(src, bad) {
				errs = append(errs, name+" has renamed CSS property "+bad+"; replace host- with box-")
			}
		}
	}
	return errs
}

func TestNoRenamedCSSProperties(t *testing.T) {
	for _, msg := range checkNoRenamedCSSProperties(webAssetsByExt(t, ".css", ".html", ".js")) {
		t.Error(msg)
	}
	// Replay past bug c8a6f7d on a template previously omitted from the hardcoded map.
	if got := checkNoRenamedCSSProperties(map[string]string{"proposals.html": ".card { host-shadow: 0 1px 2px; }"}); len(got) != 1 {
		t.Fatalf("replay c8a6f7d: got %v, want 1 renamed-property finding", got)
	}
}
