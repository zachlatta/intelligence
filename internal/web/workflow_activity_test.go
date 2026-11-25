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
	t.Parallel()
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Test activity")
	unchangedQuery := q("SELECT (to_jsonb(r)-'current_activity'-'activity_updated_at'-'activity_eta'-'activity_eta_estimated'-'activity_waiting_on'-'updated_at')::text FROM workflow_runs r WHERE id='" + run + "'")
	_, before := sqlPost(t, h, unchangedQuery)
	const line = "Holding in the airline chat queue."
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
		// the ETA shows in the viewer's zone, not the server's
		if !strings.Contains(page, `ETA <time datetime="2026-12-01T22:00:00Z" data-format="minute"`) || !strings.Contains(page, `/localtime.js`) || strings.Contains(page, "const localTimes") {
			t.Fatalf("ETA not localized on %s", path)
		}
		if path == "/workflows" && (!strings.Contains(page, `class="table runs-table"`) || strings.Contains(page, ".runs-table tr:first-child")) {
			t.Fatal("paged runs must retain activity on the first row on phones")
		}
	}

	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET current_activity='Legacy progress since 23:00 EST' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, ">Legacy progress</div>") {
			t.Fatal("legacy time not stripped", path)
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
	t.Parallel()
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

func TestActivityHistoryEstimates(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	def := workflow.Starting
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Steps[5].ID = "step-1"
	wid := createWorkflow(t, h, def)
	history := startRun(t, h, wid, "History")
	run := startRun(t, h, wid, "Estimate")
	// Only successful, recent completed step visits are useful training data.
	for i := 0; i <= len(def.Steps); i++ {
		kind := "next"
		if i == 0 {
			kind = "workspace"
		}
		if i == len(def.Steps) {
			kind = "done"
		}
		var to any
		if i < len(def.Steps) {
			to = i
		}
		_, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,kind,to_step,at) SELECT id,account_id,$2,$3,now()-interval '2 hours'+$4*interval '10 minutes' FROM workflow_runs WHERE id=$1`, history, kind, to, i)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded',finished_at=now() WHERE id=$1`, history); err != nil {
		t.Fatal(err)
	}
	for i, step := range def.Steps {
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET step=$2 WHERE id=$1`, run, i); err != nil {
			t.Fatal(err)
		}
		if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Checking tests since 23:00 EST."}); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		_, r := sqlPost(t, h, q("SELECT current_activity,activity_eta_estimated,activity_waiting_on,extract(epoch from (activity_eta-activity_updated_at)) FROM workflow_runs WHERE id='"+run+"'"))
		if len(r.Rows) != 1 {
			t.Fatalf("missing SQL row: %+v", r)
		}
		row := r.Rows[0]
		if row[0] != "Checking tests" {
			t.Fatal(row)
		}
		if step.Kind == workflow.KindSignoff {
			if row[1] != false || row[2] != "Zach" || row[3] != nil {
				t.Fatal(step.ID, row)
			}
		} else {
			remaining := len(def.Steps) - i
			if i < 4 {
				remaining--
			} // Human sign-off has no predictable duration.
			if row[1] != true || row[2] != "" || (row[3].(float64) < float64(remaining*600)-1 || row[3].(float64) > float64(remaining*600)+1) {
				t.Fatal(step.ID, row)
			}
			page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
			if !strings.Contains(page, "(estimate)") || !strings.Contains(page, "about ") {
				t.Fatal("estimate not displayed")
			}
		}
	}

	other := startRun(t, h, wid, "Other history")
	_, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,kind,to_step,at)
 SELECT $2,account_id,kind,to_step,now()-interval '1 day'+(at-(SELECT min(at) FROM workflow_run_events WHERE run_id=$1))*3
 FROM workflow_run_events WHERE run_id=$1 ORDER BY id`, history, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded',finished_at=now(),repo='/srv/other' WHERE id=$1`, other); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET repo='/srv/primary' WHERE id=$1`, history); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		repo    string
		seconds float64
	}{{"/srv/primary", 600}, {"/srv/other", 1800}, {"/srv/new", 1200}} {
		if _, err = d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET repo=$2 WHERE id=$1`, run, c.repo); err != nil {
			t.Fatal(err)
		}
		if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Landing"}); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		_, r := sqlPost(t, h, q("SELECT extract(epoch from activity_eta-activity_updated_at) FROM workflow_runs WHERE id='"+run+"'"))
		got := r.Rows[0][0].(float64)
		if got < c.seconds-1 || got > c.seconds+1 {
			t.Fatalf("repo %s: %v", c.repo, got)
		}
	}

	if _, err = d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET repo='/srv/primary' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,kind,to_step,at) SELECT id,account_id,'next',6,now()-interval '5 minutes' FROM workflow_runs WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Landing"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	_, elapsed := sqlPost(t, h, q("SELECT extract(epoch from activity_eta-activity_updated_at) FROM workflow_runs WHERE id='"+run+"'"))
	seconds := elapsed.Rows[0][0].(float64)
	if seconds < 298 || seconds > 301 {
		t.Fatalf("current elapsed time not deducted: %v", seconds)
	}
	if _, err = d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET finished_at=now()-interval '31 days' WHERE id IN ($1,$2)`, history, other); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Landing"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	_, expired := sqlPost(t, h, q("SELECT activity_eta,activity_eta_estimated FROM workflow_runs WHERE id='"+run+"'"))
	if expired.Rows[0][0] != nil || expired.Rows[0][1] != false {
		t.Fatal("used old history", expired)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Waiting for review", "waitingOn": "reviewer"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if !strings.Contains(page, "waiting on reviewer") {
		t.Fatal("external wait missing")
	}
	fresh := startRun(t, h, createWorkflow(t, h, def), "No history")
	if code, msg := move(t, h, key, fresh, map[string]string{"move": "status", "now": "Building"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	_, r := sqlPost(t, h, q("SELECT activity_eta,activity_eta_estimated FROM workflow_runs WHERE id='"+fresh+"'"))
	if r.Rows[0][0] != nil || r.Rows[0][1] != false {
		t.Fatal(r)
	}
}

func TestLegacyActivityEvent(t *testing.T) {
	t.Parallel()
	got := (runEvent{Kind: "status", Text: "Building since 23:00 EST · ETA unknown"}).Line()
	if got != "Activity: Building · ETA unknown" {
		t.Fatal(got)
	}
}
