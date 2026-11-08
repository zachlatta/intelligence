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
	for _, want := range []string{"--accent", ".topnav", ".card", ".badge", ".btn", "prefers-color-scheme: dark"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("ui.css misses %q", want)
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
		{"/usage", `href="/usage" aria-current="page"`},
		{"/resources", `href="/resources" aria-current="page"`},
		{"/login", `href="/" aria-current="page"`},
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
		for _, tab := range []string{">Hosts<", ">Plans<", ">Resources<", ">Account<"} { // one tab each
			if n := strings.Count(nav, tab); n != 1 {
				t.Errorf("%s: %d tabs %s, want 1", p.path, n, tab)
			}
		}
		for _, want := range []string{`<link rel="stylesheet" href="/ui.css`, `class="topnav"`, p.tab} {
			if !strings.Contains(page, want) {
				t.Errorf("%s misses %q", p.path, want)
			}
		}
		if strings.Count(page, `aria-current="page"`) != 1 {
			t.Errorf("%s should mark exactly one tab", p.path)
		}
	}
}
