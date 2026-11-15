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

func TestRunStatusTabsFilterBeforeLimitAndCountAllRuns(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	statuses := []string{"finding_project", "starting", "running", "queued", "waiting", "succeeded", "failed", "canceled"}
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
	groups := map[string][]string{"all": statuses, "running": statuses[:3], "queued": {"queued"}, "waiting": {"waiting"}, "completed": {"succeeded"}, "failed": {"failed"}, "canceled": {"canceled"}}
	for filter, wanted := range groups {
		page := get(t, h, "/workflows?status="+filter, nil)
		if page.Code != 200 {
			t.Fatalf("%s: %d %s", filter, page.Code, page.Body)
		}
		html := page.Body.String()
		for key, count := range map[string]int{"all": 113, "running": 3, "queued": 1, "waiting": 1, "completed": 106, "failed": 1, "canceled": 1} {
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
		table := strings.Split(html, `<table class="table">`)
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
