package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRunActivityAPIAndDisplay(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Test activity")
	unchangedQuery := q("SELECT (to_jsonb(r)-'current_activity'-'activity_updated_at'-'activity_eta'-'updated_at')::text FROM workflow_runs r WHERE id='" + run + "'")
	_, before := sqlPost(t, h, unchangedQuery)
	const line = "Holding in the airline chat queue since 20:06 UTC."
	for _, body := range []map[string]string{
		{"move": "status"}, {"move": "status", "now": "two\nlines"},
		{"move": "status", "now": strings.Repeat("x", 281)},
		{"move": "status", "now": line, "eta": "tomorrow"},
	} {
		if code, msg := move(t, h, key, run, body); code != 400 {
			t.Fatalf("invalid: %d %s", code, msg)
		}
	}
	body := map[string]string{"move": "status", "now": line, "eta": "2026-12-01T22:00:00Z"}
	if code, _ := move(t, h, "", run, body); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	if code, _ := move(t, h, keyOf(t, h, "host-2"), run, body); code != 404 {
		t.Fatal(code)
	}
	if code, msg := move(t, h, key, run, body); code != 200 {
		t.Fatalf("status: %d %s", code, msg)
	}
	_, after := sqlPost(t, h, unchangedQuery)
	if len(before.Rows) != 1 || len(after.Rows) != 1 || before.Rows[0][0] != after.Rows[0][0] {
		t.Fatal("status changed run lifecycle")
	}
	code, result := sqlPost(t, h, q("SELECT current_activity, activity_updated_at IS NOT NULL, activity_eta IS NOT NULL FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || len(result.Rows) != 1 || result.Rows[0][0] != line || result.Rows[0][1] != true || result.Rows[0][2] != true {
		t.Fatalf("SQL: %+v", result)
	}
	_, events := sqlPost(t, h, q("SELECT text FROM workflow_run_events WHERE run_id='"+run+"' AND kind='status'"))
	if len(events.Rows) != 1 || !strings.Contains(events.Rows[0][0].(string), line) || !strings.Contains(events.Rows[0][0].(string), "2026-12-01") {
		t.Fatalf("events: %+v", events)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, line) || !strings.Contains(page, "ETA") || !strings.Contains(page, "Updated") || strings.Contains(page, "Stale · Updated") {
			t.Fatalf("missing fresh activity on %s", path)
		}
	}
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET activity_updated_at=now()-interval '31 minutes',status='waiting' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
		if !strings.Contains(get(t, h, path, nil).Body.String(), "Stale · Updated") {
			t.Fatal("missing stale activity", path)
		}
	}
	delete(body, "eta")
	if code, msg := move(t, h, key, run, body); code != 200 {
		t.Fatalf("waiting update: %d %s", code, msg)
	}
	_, result = sqlPost(t, h, q("SELECT activity_eta FROM workflow_runs WHERE id='"+run+"'"))
	if result.Rows[0][0] != nil {
		t.Fatal("omitting ETA must clear previous estimate")
	}
	call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch)
	if code, _ := move(t, h, key, run, body); code != 409 {
		t.Fatal("ended run accepted status", code)
	}
}

func TestActivityStaleBoundary(t *testing.T) {
	now := time.Now()
	for _, age := range []time.Duration{0, 30 * time.Minute, 30*time.Minute + time.Second} {
		at := now.Add(-age)
		if got := (runRow{ActivityUpdatedAt: &at}).ActivityStale(now); got != (age > 30*time.Minute) {
			t.Fatalf("age %v stale %v", age, got)
		}
	}
	if (runRow{}).ActivityStale(now) {
		t.Fatal("absent activity is not stale")
	}
}
