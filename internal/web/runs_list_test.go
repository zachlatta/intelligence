package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTheRunsListPagesThroughHistoryAndSearchesIt: /workflows shows the newest runs a page at a
// time, with links to older ones, and its search box finds runs in all of history, not only the
// page on screen.
func TestTheRunsListPagesThroughHistoryAndSearchesIt(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h)
	// 120 finished runs, "Old run 001" the oldest and "Old run 120" the newest, a minute apart
	start := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status,
		created_at, updated_at, finished_at)
		SELECT account_id, id, 1, 'host-1', 'Old run ' || lpad(i::text, 3, '0'), 'input of run ' || i, 'succeeded',
			$1::timestamptz + i * interval '1 minute', $1::timestamptz + i * interval '1 minute', $1::timestamptz + i * interval '1 minute'
		FROM workflows, generate_series(1, 120) i WHERE id = $2`, start, wf); err != nil {
		t.Fatal(err)
	}
	// the oldest: a summary, a prompt with a rare word, and a waiting run deep in history
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET task_summary = 'Fix the billing export',
		input = 'Please fix the zanzibar report, 100% of it' WHERE title = 'Old run 001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status = 'waiting', note = 'Which one?' WHERE title = 'Old run 002'`); err != nil {
		t.Fatal(err)
	}
	page := func(path string) string {
		t.Helper()
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	runs := func(body string) string {
		_, after, _ := strings.Cut(body, `<div id="runs">`)
		before, _, _ := strings.Cut(after, "<script>")
		return before
	}
	has := func(body string, n int) bool {
		label := fmt.Sprintf("Old run %03d<", n)
		if n == 1 {
			label = "Fix the billing export<" // its summary names it
		}
		return strings.Contains(runs(body), label)
	}

	// page 1: the newest 50, a link to older ones and none to newer
	first := page("/workflows")
	if !has(first, 120) || !has(first, 71) || has(first, 70) {
		t.Error("page 1 should show runs 120 to 71")
	}
	if !strings.Contains(runs(first), `href="/workflows?page=2"`) || strings.Contains(runs(first), "Newer") {
		t.Error("page 1 should link to page 2 and not to newer runs")
	}
	if !strings.Contains(runs(first), "1–50 of 120") {
		t.Error("page 1 should say which runs it shows")
	}
	// the waiting run's note still shows, though the run is on the last page
	if !strings.Contains(first, "is asking you: “Old run 002”") {
		t.Error("a waiting run on a later page lost its banner")
	}
	// the search box, a GET form on the page itself
	if !strings.Contains(first, `<form class="runs-search" method="get" action="/workflows" role="search">`) ||
		!strings.Contains(first, `name="q"`) {
		t.Error("no search box")
	}

	second := page("/workflows?page=2")
	if !has(second, 70) || !has(second, 21) || has(second, 71) || has(second, 20) {
		t.Error("page 2 should show runs 70 to 21")
	}
	if !strings.Contains(runs(second), `href="/workflows"`) || !strings.Contains(runs(second), `href="/workflows?page=3"`) {
		t.Error("page 2 should link to pages 1 and 3")
	}
	last := page("/workflows?page=3")
	if !has(last, 20) || !has(last, 1) || strings.Contains(runs(last), "page=4") || !strings.Contains(runs(last), "101–120 of 120") {
		t.Error("page 3 should show runs 20 to 1 and stop")
	}
	if !strings.Contains(page("/workflows?page=9"), "No runs on this page") {
		t.Error("a page past the end should say so")
	}
	for _, bad := range []string{"0", "-1", "x"} {
		if !has(page("/workflows?page="+bad), 120) {
			t.Errorf("page=%s should read as page 1", bad)
		}
	}

	// search reaches all history: the summary, the prompt, the workflow, the status
	for q, want := range map[string]int{
		"billing":           1,
		"ZANZIBAR":          1,
		"zanzibar report":   1,
		"100%":              1,
		"run 042":           42,
		"waiting":           2,
		"input of run 107 ": 107,
	} {
		body := page("/workflows?q=" + url.QueryEscape(q))
		if !has(body, want) {
			t.Errorf("search %q did not find run %d", q, want)
		}
		if strings.Count(runs(body), `<a href="/workflow-runs/`) != 1 {
			t.Errorf("search %q found %d runs, want 1", q, strings.Count(runs(body), `<a href="/workflow-runs/`))
		}
	}
	// "%" and "_" are literal, not wildcards; the box keeps what was typed
	if body := page("/workflows?q=1_0"); strings.Contains(runs(body), `<a href="/workflow-runs/`) || !strings.Contains(body, `value="1_0"`) {
		t.Error("_ should match only itself, and the box should keep the search")
	}
	if !strings.Contains(page("/workflows?q=nothing+like+it"), "No runs match") {
		t.Error("an empty search should say so")
	}
	// a search's pages keep the search
	all := page("/workflows?q=old+run")
	if !strings.Contains(runs(all), `href="/workflows?page=2&amp;q=old&#43;run"`) && !strings.Contains(runs(all), `href="/workflows?page=2&amp;q=old+run"`) {
		t.Errorf("a search's next page should keep the search: %s", runs(all))
	}

	// another account sees none of them
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, http.MethodGet, "/workflows?q=old", "", nil, sessionFor(t, ts.store, bob))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Old run") {
		t.Error("bob found alice's runs")
	}
}
