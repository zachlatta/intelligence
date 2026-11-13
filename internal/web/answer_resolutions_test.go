package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// answerRuns starts n email-triggered runs (each expects an answer), requested an hour ago and
// finished, so each shows in workflow_runs_unanswered until answered or resolved.
func answerRuns(t *testing.T, n int) (h http.Handler, runs []string, key string, d dbtest.DB) {
	t.Helper()
	srv, d := dbServer(t)
	sendReport(t, srv, wfReport())
	def := workflow.Starting
	def.Trigger = workflow.TriggerEmail
	def.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
	wf := createWorkflow(t, srv, def)
	for i := range n {
		run := startRun(t, srv, wf, (workflow.Mail{From: "requester@example.com", Subject: fmt.Sprintf("Example request %d", i), Date: "Wed, 11 Nov 2026 14:39:00 -0500", Text: "Please answer."}).Input(true))
		if _, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET requested_at=now()-interval '1 hour', status='succeeded', finished_at=now() WHERE id=$1`, run); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	return srv, runs, keyOf(t, srv, "host-1"), d
}

func resolve(t *testing.T, h http.Handler, key, run, body string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answer-resolution", body, map[string]string{"Authorization": "Bearer " + key})
}

func unanswered(t *testing.T, h http.Handler) []string {
	t.Helper()
	code, r := sqlPost(t, h, q(`SELECT id::text FROM workflow_runs_unanswered ORDER BY id`))
	if code != 200 {
		t.Fatalf("unanswered: %d %+v", code, r)
	}
	var ids []string
	for _, row := range r.Rows {
		ids = append(ids, row[0].(string))
	}
	return ids
}

func TestSteeredRunIsAnsweredByTheRunItWasHandedTo(t *testing.T) {
	h, runs, key, d := answerRuns(t, 4)
	a, b, c, other := runs[0], runs[1], runs[2], runs[3]
	steer := func(from, to, at string) *httptest.ResponseRecorder {
		return resolve(t, h, key, from, `{"kind":"steered","steeredTo":"`+to+`","at":"`+at+`"}`)
	}
	if rec := steer(a, a, "2026-11-12T18:00:00Z"); rec.Code != 400 {
		t.Fatalf("steered to itself: %d %s", rec.Code, rec.Body)
	}
	if rec := steer(a, "11111111-1111-4111-8111-111111111111", "2026-11-12T18:00:00Z"); rec.Code != 400 {
		t.Fatalf("steered to a run that does not exist: %d %s", rec.Code, rec.Body)
	}
	if rec := resolve(t, h, keyOf(t, h, "host-2"), a, `{"kind":"steered","steeredTo":"`+b+`"}`); rec.Code != 404 {
		t.Fatalf("another host's run: %d", rec.Code)
	}
	if rec := resolve(t, h, key, a, `{"kind":"legacy","reason":"agents cannot mark runs legacy"}`); rec.Code != 400 {
		t.Fatalf("legacy through the API: %d", rec.Code)
	}
	// a -> b -> c: a chain is followed to the run that answers.
	for i := 0; i < 2; i++ { // an identical retry is accepted
		if rec := steer(a, b, "2026-11-12T18:00:00Z"); rec.Code != 200 {
			t.Fatalf("steer a: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := steer(a, c, "2026-11-12T18:00:00Z"); rec.Code != 400 {
		t.Fatalf("rewrote a's steer: %d", rec.Code)
	}
	if rec := steer(b, c, "2026-11-12T18:10:00Z"); rec.Code != 200 {
		t.Fatalf("steer b: %d %s", rec.Code, rec.Body)
	}
	if rec := steer(c, a, "2026-11-12T18:20:00Z"); rec.Code != 400 {
		t.Fatalf("accepted a steering loop: %d", rec.Code)
	}
	// Until c answers, all three still owe an answer, and the view says where a and b went.
	if got := unanswered(t, h); !slices.Equal(got, sorted(a, b, c, other)) {
		t.Fatalf("before c answers: %v", got)
	}
	code, r := sqlPost(t, h, q(`SELECT steered_to::text FROM workflow_runs_unanswered WHERE id='`+a+`'`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != b {
		t.Fatalf("steered_to: %+v", r)
	}
	// a waits while its chain ends in a live run (c); the fixture's runs are all finished
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='running', finished_at=NULL WHERE id=$1`, c); err != nil {
		t.Fatal(err)
	}
	if page := get(t, h, "/workflow-runs/"+a, nil).Body.String(); !strings.Contains(page, "Steered to run "+b[:8]+"; awaiting") {
		t.Fatal("run page does not say where the request went")
	}
	// An answer c sent before b's request reached it does not answer b (or a, which came through b).
	answer := func(run, id, at string) {
		rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", `{"channel":"email","messageId":"`+id+`","sentAt":"`+at+`","checked":true}`, map[string]string{"Authorization": "Bearer " + key})
		if rec.Code != 200 {
			t.Fatalf("answer: %s", rec.Body)
		}
	}
	answer(c, "early", "2026-11-12T18:05:00Z")
	if got := unanswered(t, h); !slices.Equal(got, sorted(a, b, other)) {
		t.Fatalf("after c's early answer: %v", got)
	}
	answer(c, "folded-in", "2026-11-12T19:24:00Z")
	if got := unanswered(t, h); !slices.Equal(got, []string{other}) {
		t.Fatalf("after c's answer: %v", got)
	}
	code, r = sqlPost(t, h, q(`SELECT answered_via::text, resolution, answered_at=TIMESTAMPTZ '2026-11-12T19:24:00Z' FROM workflow_run_answer_status WHERE id='`+a+`'`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != c || r.Rows[0][1] != "steered" || r.Rows[0][2] != true {
		t.Fatalf("answer status: %+v", r)
	}
	if page := get(t, h, "/workflow-runs/"+a, nil).Body.String(); !strings.Contains(page, "(via run "+c[:8]+")") || strings.Contains(page, "No answer recorded") {
		t.Fatal("run page does not credit the receiving run's answer")
	}
}

func TestNotNeededNeedsAReasonAndLeavesTheView(t *testing.T) {
	h, runs, key, d := answerRuns(t, 2)
	for _, body := range []string{`{"kind":"not_needed"}`, `{"kind":"not_needed","reason":"   "}`, `{"kind":"not_needed","reason":"` + strings.Repeat("a", 2001) + `"}`, `{"kind":"other","reason":"x"}`} {
		if rec := resolve(t, h, key, runs[0], body); rec.Code != 400 {
			t.Fatalf("accepted %.60s: %d", body, rec.Code)
		}
	}
	if rec := resolve(t, h, key, runs[0], `{"kind":"not_needed","reason":"A recorded meeting, not a request."}`); rec.Code != 200 {
		t.Fatalf("not needed: %d %s", rec.Code, rec.Body)
	}
	if got := unanswered(t, h); !slices.Equal(got, []string{runs[1]}) {
		t.Fatalf("unanswered: %v", got)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + runs[0]} {
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, "No answer needed") {
			t.Fatalf("%s does not show the run needed no answer", path)
		}
	}
	if page := get(t, h, "/workflow-runs/"+runs[0], nil).Body.String(); !strings.Contains(page, "A recorded meeting, not a request.") {
		t.Fatal("run page omits the reason")
	}
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "other-reader", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"workflow_run_answer_resolutions", "workflow_run_answer_status"} {
		rec := call(t, h, "GET", "/api/sql?q=SELECT+count(*)+FROM+"+table, "", nil, sessionFor(t, ts.store, bob))
		if !strings.Contains(rec.Body.String(), `"rows":[[0]]`) {
			t.Fatalf("cross-account read of %s: %s", table, rec.Body)
		}
	}
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestRunsFromBeforeAnswerRecordingAreMarkedLegacyOnce(t *testing.T) {
	h, runs, key, d := answerRuns(t, 4)
	old, oldAnswered, oldTask, recent := runs[0], runs[1], runs[2], runs[3]
	rec := call(t, anon(h), "POST", "/api/workflow-runs/"+oldAnswered+"/answers", `{"channel":"email","messageId":"sent","checked":true}`, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET created_at=(SELECT applied_at FROM schema_migrations WHERE version='0047_workflow_answers')-interval '1 day'
 WHERE id = ANY($1::uuid[])`, []string{old, oldAnswered, oldTask}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET answer_expected=false WHERE id=$1`, oldTask); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `DROP VIEW workflow_runs_unanswered; DROP VIEW workflow_run_answer_status; DROP TABLE workflow_run_answer_resolutions;
 DROP TABLE workflow_run_claims;
 DELETE FROM schema_migrations WHERE version IN ('0050_workflow_answer_resolutions', '20261113T1617_workflow_run_claims')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d.Owner); err != nil {
		t.Fatal(err)
	}
	code, r := sqlPost(t, h, q(`SELECT run_id::text, kind FROM workflow_run_answer_resolutions`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != old || r.Rows[0][1] != "legacy" {
		t.Fatalf("legacy marks: %+v", r)
	}
	if got := unanswered(t, h); !slices.Equal(got, []string{recent}) {
		t.Fatalf("unanswered: %v", got)
	}
	if page := get(t, h, "/workflow-runs/"+old, nil).Body.String(); !strings.Contains(page, "Before answer recording") || strings.Contains(page, "No answer recorded") {
		t.Fatal("run page flags a legacy run as unanswered")
	}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}
