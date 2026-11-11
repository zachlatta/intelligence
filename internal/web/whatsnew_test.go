package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/whatsnew"
)

// unseenIDs is what the account behind cookie has not seen, read as the browser does: through /api/sql.
func unseenIDs(t *testing.T, h http.Handler, cookies ...*http.Cookie) []string {
	t.Helper()
	r := call(t, h, "POST", "/api/sql", `{"query":"SELECT id FROM whats_new_unseen ORDER BY released_on DESC, position"}`, nil, cookies...)
	if r.Code != 200 {
		t.Fatalf("sql: %d %s", r.Code, r.Body)
	}
	var body struct{ Rows [][]string }
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, row := range body.Rows {
		ids = append(ids, row[0])
	}
	return ids
}

func TestWhatsNew(t *testing.T) {
	h, d := dbServer(t)
	if err := whatsnew.Sync(context.Background(), d.Owner, []whatsnew.Entry{
		{ID: "newer", Released: "2026-11-02", Title: "A newer thing", Summary: "It does <b>more</b>.", Media: "whats-new.gif", MediaAlt: "the newer thing"},
		{ID: "older", Released: "2026-11-01", Title: "An older thing", Summary: "It did less."},
	}); err != nil {
		t.Fatal(err)
	}

	// every page's top bar links to it, and loads the script that marks it when something is unseen
	home := call(t, h, "GET", "/", "", nil)
	if !strings.Contains(home.Body.String(), `href="/whats-new"`) || !strings.Contains(home.Body.String(), `/whatsnew.js`) ||
		!strings.Contains(home.Body.String(), `<span class="unseen-count" aria-hidden="true" hidden></span>`) {
		t.Fatalf("home page has no What's New link or script:\n%s", home.Body)
	}
	if r := call(t, h, "GET", "/whatsnew.js", "", nil); r.Code != 200 || !strings.Contains(r.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(r.Body.String(), "whats_new_unseen") || !strings.Contains(r.Body.String(), "/api/whats-new/seen") {
		t.Fatalf("whatsnew.js: %d %s", r.Code, r.Header())
	}

	if got := unseenIDs(t, h); strings.Join(got, ",") != "newer,older" {
		t.Fatalf("unseen before reading: %v", got)
	}

	// the page lists them newest first, marks the unseen, escapes what it shows and shows the demo
	page := call(t, h, "GET", "/whats-new", "", nil)
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("page: %d %s", page.Code, body)
	}
	newer, older := strings.Index(body, "A newer thing"), strings.Index(body, "An older thing")
	if newer < 0 || older < 0 || newer > older {
		t.Fatalf("entries missing or out of order:\n%s", body)
	}
	for _, want := range []string{`It does &lt;b&gt;more&lt;/b&gt;.`, `src="/whats-new/media/whats-new.gif"`, `alt="the newer thing"`,
		`data-unseen="newer"`, `data-unseen="older"`, `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	// reading the page writes nothing: the browser marks what it showed through the API
	if got := unseenIDs(t, h); len(got) != 2 {
		t.Fatalf("a page read marked entries seen: %v", got)
	}
	if r := call(t, anon(h), "GET", "/whats-new", "", nil); r.Code != http.StatusSeeOther {
		t.Fatalf("anonymous page: %d", r.Code)
	}

	// the demo is served as itself
	if r := call(t, anon(h), "GET", "/whats-new/media/whats-new.gif", "", nil); r.Code != 200 || r.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("media: %d %s", r.Code, r.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/whats-new/media/nope.gif", "/whats-new/media/..%2fwhatsnew.go"} {
		if r := call(t, anon(h), "GET", p, "", nil); r.Code != 404 {
			t.Errorf("%s: %d", p, r.Code)
		}
	}

	// marking seen is an API write: same-origin JSON from a signed-in account, of entries that exist
	for _, tc := range []struct {
		h      http.Handler
		body   string
		header map[string]string
		code   int
	}{
		{anon(h), `{"ids":["newer"]}`, sameOriginFetch, 401},
		{h, `{"ids":["newer"]}`, map[string]string{"Origin": "https://evil.example"}, 403},
		{h, `{"ids":["nope"]}`, sameOriginFetch, 400},
		{h, `{"ids":["newer","nope"]}`, sameOriginFetch, 400},
		{h, `{"ids":[]}`, sameOriginFetch, 400},
		{h, `{`, sameOriginFetch, 400},
	} {
		if r := call(t, tc.h, "POST", "/api/whats-new/seen", tc.body, tc.header); r.Code != tc.code {
			t.Errorf("%s: %d, want %d (%s)", tc.body, r.Code, tc.code, r.Body)
		}
	}
	if got := unseenIDs(t, h); strings.Join(got, ",") != "newer,older" {
		t.Fatalf("a rejected request marked entries seen: %v", got)
	}
	for range 2 { // marking twice is fine
		if r := call(t, h, "POST", "/api/whats-new/seen", `{"ids":["newer"]}`, sameOriginFetch); r.Code != http.StatusNoContent {
			t.Fatalf("mark seen: %d %s", r.Code, r.Body)
		}
	}
	if got := unseenIDs(t, h); strings.Join(got, ",") != "older" {
		t.Fatalf("unseen after marking newer: %v", got)
	}
	if body := call(t, h, "GET", "/whats-new", "", nil).Body.String(); strings.Contains(body, `data-unseen="newer"`) || !strings.Contains(body, `data-unseen="older"`) {
		t.Fatalf("page still marks newer unseen:\n%s", body)
	}

	// what one account has seen is its own
	ts := h.(*testServer)
	other, err := ts.store.Create(context.Background(), "someone-else", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	otherCookie := sessionFor(t, ts.store, other)
	if got := unseenIDs(t, anon(h), otherCookie); strings.Join(got, ",") != "newer,older" {
		t.Fatalf("another account's unseen: %v", got)
	}
	r := call(t, h, "POST", "/api/sql", `{"query":"SELECT count(*) FROM whats_new_seen"}`, nil)
	if !strings.Contains(r.Body.String(), `[["1"]]`) && !strings.Contains(r.Body.String(), `[[1]]`) {
		t.Fatalf("own seen rows: %s", r.Body)
	}
	r = call(t, anon(h), "POST", "/api/sql", `{"query":"SELECT count(*) FROM whats_new_seen"}`, nil, otherCookie)
	if !strings.Contains(r.Body.String(), `[["0"]]`) && !strings.Contains(r.Body.String(), `[[0]]`) {
		t.Fatalf("another account reads seen rows not its own: %s", r.Body)
	}
}
