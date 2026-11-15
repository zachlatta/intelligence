package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func activityRevisionRun(t *testing.T) (*testServer, *fakeClock, string, string) {
	t.Helper()
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	wid := createWorkflow(t, h, workflow.Starting)
	history := startRun(t, h, wid, "Completed checks")
	run := startRun(t, h, wid, "Current checks")
	for i := 0; i <= len(workflow.Starting.Steps); i++ {
		kind := "next"
		if i == 0 {
			kind = "workspace"
		}
		var to any = i
		if i == len(workflow.Starting.Steps) {
			kind = "done"
			to = nil
		}
		_, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,kind,to_step,at)
 SELECT id,account_id,$2,$3,$4 FROM workflow_runs WHERE id=$1`, history, kind, to, clock.Now().Add(-3*time.Hour+time.Duration(i)*10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded',finished_at=$2 WHERE id=$1`, history, clock.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='running',step=0,orchestrator_id='orchestrator' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,kind,to_step,at) SELECT id,account_id,'workspace',0,$2 FROM workflow_runs WHERE id=$1`, run, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Checking changes"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	return h, clock, key, run
}

func assertActivityRemaining(t *testing.T, h *testServer, clock *fakeClock, run string, seconds float64) {
	t.Helper()
	var remaining *float64
	var estimated bool
	err := h.d.Owner.QueryRow(t.Context(), `SELECT extract(epoch from (activity_eta-$2::timestamptz))::double precision,activity_eta_estimated FROM workflow_runs WHERE id=$1`, run, clock.Now()).Scan(&remaining, &estimated)
	if err != nil || remaining == nil {
		t.Fatalf("missing ETA: %v", err)
	}
	if !estimated || *remaining < seconds-5 || *remaining > seconds+5 {
		t.Fatalf("remaining %.3f estimated %v, want %.0f", *remaining, estimated, seconds)
	}
}

func TestActivityEstimateTransitions(t *testing.T) {
	h, clock, key, run := activityRevisionRun(t)
	assertActivityRemaining(t, h, clock, run, 3600)
	for _, m := range []map[string]string{{"move": "next"}, {"move": "back", "step": "build", "why": "Rework"}, {"move": "skip", "why": "Already checked"}} {
		if code, msg := move(t, h, key, run, m); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		want := float64(3000)
		if m["move"] == "back" {
			want = 3600
		}
		assertActivityRemaining(t, h, clock, run, want)
	}
}

func TestActivityEstimateExpiry(t *testing.T) {
	h, clock, key, run := activityRevisionRun(t)
	clock.Add(2 * time.Hour)
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	section := strings.Split(strings.Split(page, `aria-label="Current activity">`)[1], "</section>")[0]
	if strings.Contains(section, "(estimate)") {
		t.Fatal("past estimate displayed", section)
	}

	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	assertActivityRemaining(t, h, clock, run, 3000)
	// SQL clients have the same expiry-safe projection as the pages.
	if _, err := h.d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET activity_eta=now()-interval '1 second' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	code, r := sqlPost(t, h, q("SELECT activity_eta,activity_eta_estimated FROM workflow_run_activity WHERE run_id='"+run+"'"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != nil || r.Rows[0][1] != false {
		t.Fatalf("expired SQL projection: %d %+v", code, r)
	}
}

func TestActivityReplyClearsResolvedWait(t *testing.T) {
	h, clock, key, run := activityRevisionRun(t)
	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "Review this"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if !strings.Contains(get(t, h, "/workflow-runs/"+run, nil).Body.String(), "waiting on Zach") {
		t.Fatal("ask did not display wait")
	}
	if r := call(t, h, "POST", "/api/workflow-runs/"+run+"/reply", `{"text":"Continue"}`, sameOriginFetch); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	_, r := sqlPost(t, h, q("SELECT activity_waiting_on FROM workflow_runs WHERE id='"+run+"'"))
	if r.Rows[0][0] != "" {
		t.Fatal("resolved wait remains", r)
	}
	assertActivityRemaining(t, h, clock, run, 3000)
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	section := strings.Split(strings.Split(page, `aria-label="Current activity">`)[1], "</section>")[0]
	if strings.Contains(section, "waiting on Zach") || !strings.Contains(section, "(estimate)") {
		t.Fatal("reply activity display", section)
	}
}

func TestActivitySendBackUsesCurrentVisit(t *testing.T) {
	h, clock, key, run := activityRevisionRun(t)
	if _, err := h.d.Owner.Exec(t.Context(), `UPDATE workflow_run_events SET at=$2 WHERE run_id=$1 AND kind='workspace'`, run, clock.Now().Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "back", "step": "build", "why": "Rework"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Reworking"}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	assertActivityRemaining(t, h, clock, run, 3600)
}

func TestActivityTransitionsPreserveAgentETA(t *testing.T) {
	h, clock, key, run := activityRevisionRun(t)
	eta := clock.Now().Add(4 * time.Hour).Format(time.RFC3339)
	if code, msg := move(t, h, key, run, map[string]string{"move": "status", "now": "Checking", "eta": eta}); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	for _, m := range []map[string]string{{"move": "next"}, {"move": "back", "step": "build", "why": "Rework"}, {"move": "ask", "note": "Review this"}} {
		if code, msg := move(t, h, key, run, m); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		var at time.Time
		var estimated bool
		if err := h.d.Owner.QueryRow(t.Context(), `SELECT activity_eta,activity_eta_estimated FROM workflow_runs WHERE id=$1`, run).Scan(&at, &estimated); err != nil || at.UTC().Format(time.RFC3339) != eta || estimated {
			t.Fatalf("agent ETA changed: %v %v %v", at, estimated, err)
		}
	}
	if r := call(t, h, "POST", "/api/workflow-runs/"+run+"/reply", `{"text":"Continue"}`, sameOriginFetch); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	_, r := sqlPost(t, h, q("SELECT activity_eta IS NOT NULL,activity_eta_estimated,activity_waiting_on FROM workflow_run_activity WHERE run_id='"+run+"'"))
	if r.Rows[0][0] != true || r.Rows[0][1] != false || r.Rows[0][2] != "" {
		t.Fatal("agent ETA not restored", r)
	}
}
