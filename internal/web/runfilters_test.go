package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWaitingRunBannerIgnoresStatusFilter(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Review this task")
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='waiting' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"completed", "all", "running", "queued", "waiting", "failed", "canceled", "stalled"} {
		page := get(t, h, "/workflows?status="+status, nil)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "is asking you:") || !strings.Contains(page.Body.String(), `href="/workflow-runs/`+run+`#reply">Read the note and reply`) {
			t.Fatalf("%s: waiting run's banner is missing", status)
		}
	}
	bob, err := h.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	page := call(t, h, http.MethodGet, "/workflows?status=completed", "", nil, sessionFor(t, h.store, bob))
	if strings.Contains(page.Body.String(), "is asking you:") {
		t.Fatal("other account can see waiting banner")
	}
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='succeeded' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	page = get(t, h, "/workflows?status=completed", nil)
	if strings.Contains(page.Body.String(), "is asking you:") {
		t.Fatal("resolved waiting banner remains in refreshed page")
	}
}

func TestRunStatusTabsFilterBeforeLimitAndCountAllRuns(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	statuses := []string{"finding_project", "starting", "running", "queued", "waiting", "succeeded", "failed", "canceled", "stalled"}
	ids := map[string]string{}
	for _, status := range statuses {
		id := startRun(t, h, wf, "Example "+status)
		ids[status] = id
		if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status=$2 WHERE id=$1", id, status); err != nil {
			t.Fatal(err)
		}
	}
	// More than one page of recent completed runs must not hide older active runs.
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs (workflow_id, version, account_id, host, status, title, input, created_at, updated_at)
 SELECT workflow_id, version, account_id, host, 'succeeded', 'Recent completed task', '', now(), now() FROM workflow_runs, generate_series(1,105) WHERE id=$1`, ids["succeeded"]); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{"all": statuses, "running": statuses[:3], "queued": {"queued"}, "waiting": {"waiting"}, "completed": {"succeeded"}, "failed": {"failed"}, "canceled": {"canceled"}, "stalled": {"stalled"}}
	for filter, wanted := range groups {
		page := get(t, h, "/workflows?status="+filter, nil)
		if page.Code != 200 {
			t.Fatalf("%s: %d %s", filter, page.Code, page.Body)
		}
		html := page.Body.String()
		for key, count := range map[string]int{"all": 114, "running": 3, "queued": 1, "waiting": 1, "completed": 106, "failed": 1, "canceled": 1, "stalled": 1} {
			if !strings.Contains(html, fmt.Sprintf(`data-run-status="%s"`, key)) || !strings.Contains(html, fmt.Sprintf(`data-run-count="%s">%d</span>`, key, count)) {
				t.Errorf("%s: wrong %s count", filter, key)
			}
		}
		if !strings.Contains(html, `data-run-status="`+filter+`" aria-current="page"`) {
			t.Errorf("%s not selected", filter)
		}
		if filter == "all" || filter == "completed" {
			continue
		}
		table := strings.Split(html, `<table class="table runs-table">`)
		if len(table) != 2 {
			t.Fatalf("%s: missing table", filter)
		}
		for _, status := range statuses {
			want := false
			for _, s := range wanted {
				want = want || s == status
			}
			if strings.Contains(table[1], `/workflow-runs/`+ids[status]) != want {
				t.Errorf("%s: wrong visibility for %s", filter, status)
			}
		}
	}
	if page := get(t, h, "/workflows?status=invalid", nil); !strings.Contains(page.Body.String(), `data-run-status="all" aria-current="page"`) {
		t.Error("unknown filter should select all")
	}
	bob, err := h.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	page := call(t, h, http.MethodGet, "/workflows?status=queued", "", nil, sessionFor(t, h.store, bob))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "No Queued runs.") {
		t.Fatal("missing filtered empty state")
	}
	for key := range groups {
		if !strings.Contains(page.Body.String(), fmt.Sprintf(`data-run-count="%s">0</span>`, key)) {
			t.Errorf("other account can see %s counts", key)
		}
	}
}

func TestStatusFilterKeepsSearchAndPagination(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs
		(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
		SELECT account_id,id,1,'host-1','Needle task '||i,'','succeeded',now(),now()
		FROM workflows,generate_series(1,55) i WHERE id=$1`, wf); err != nil {
		t.Fatal(err)
	}
	startRun(t, h, wf, "Needle active task")
	page := get(t, h, "/workflows?status=completed&q=Needle", nil).Body.String()
	_, history, _ := strings.Cut(page, `<div id="runs">`) // In progress above lists live runs whatever the filter
	if !strings.Contains(history, "1–50 of 55") || strings.Contains(history, ">Needle active task</a>") {
		t.Fatal("search and status must filter before pagination")
	}
	if !strings.Contains(page, `href="/workflows?page=2&amp;q=Needle&amp;status=completed"`) {
		t.Fatal("pagination lost search or status")
	}
	if !strings.Contains(page, `href="/workflows?q=Needle&amp;status=running#runs"`) {
		t.Fatal("status tab lost search or failed to reset page")
	}
	if !strings.Contains(page, `name="status" value="completed"`) || !strings.Contains(page, `href="/workflows?status=completed">Clear`) {
		t.Fatal("search form or Clear lost status")
	}
	page = get(t, h, "/workflows?status=completed&q=Needle&page=2", nil).Body.String()
	if !strings.Contains(page, "51–55 of 55") {
		t.Fatal("filtered page two is wrong")
	}
}
