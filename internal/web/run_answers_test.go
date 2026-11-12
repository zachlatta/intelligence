package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRunAnswersAndUnansweredRequests(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	def := workflow.Starting
	def.Trigger = workflow.TriggerEmail
	def.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
	wf := createWorkflow(t, h, def)
	var runs []string
	for i, status := range []string{"queued", "starting", "succeeded", "failed", "succeeded"} {
		run := startRun(t, h, wf, (workflow.Mail{From: "requester@example.com", Subject: fmt.Sprintf("Example email request %d", i), Date: "Wed, 11 Nov 2026 14:39:00 -0500", Text: "Please answer this example request."}).Input(true))
		runs = append(runs, run)
		_, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET requested_at=now()-interval '1 hour',status=$2, finished_at=CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE NULL END WHERE id=$1`, run, status)
		if err != nil {
			t.Fatal(err)
		}
	}
	post := func(key, body string) int {
		return call(t, anon(h), "POST", "/api/workflow-runs/"+runs[4]+"/answers", body, map[string]string{"Authorization": "Bearer " + key}).Code
	}
	draft := `{"channel":"email","messageId":"draft-send","threadId":"thread-demo","sentAt":"2026-11-11T20:00:00Z","checked":false}`
	if code := post("", draft); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	if code := post(keyOf(t, h, "host-2"), draft); code != 404 {
		t.Fatalf("other host: %d", code)
	}
	if code := post(key, `{"channel":"email"}`); code != 400 {
		t.Fatalf("invalid: %d", code)
	}
	for _, body := range []string{draft, draft, `{"channel":"email","messageId":"checked-send","threadId":"thread-demo","sentAt":"2026-11-11T20:30:00Z","checked":true}`} {
		if code := post(key, body); code != 200 {
			t.Fatalf("answer: %d", code)
		}
	}
	if code := post(key, strings.Replace(draft, "thread-demo", "different-thread", 1)); code != 400 {
		t.Fatalf("rewrote history: %d", code)
	}
	code, result := sqlPost(t, h, q(`SELECT message_id,checked FROM workflow_run_answers ORDER BY sent_at`))
	if code != 200 || len(result.Rows) != 2 || result.Rows[0][0] != "draft-send" || result.Rows[1][1] != true {
		t.Fatalf("answers: %d %+v", code, result)
	}
	code, result = sqlPost(t, h, q(`SELECT id FROM workflow_runs_unanswered`))
	if code != 200 || len(result.Rows) != 4 {
		t.Fatalf("unanswered: %d %+v", code, result)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + runs[3]} {
		page := get(t, h, path, nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), "No answer recorded") {
			t.Fatalf("missing warning: %s", page.Body)
		}
	}
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "other-reader", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"workflow_run_answers", "workflow_runs_unanswered"} {
		rec := call(t, h, "GET", "/api/sql?q=SELECT+count(*)+FROM+"+table, "", nil, sessionFor(t, ts.store, bob))
		if !strings.Contains(rec.Body.String(), `"rows":[[0]]`) {
			t.Fatalf("cross-account read: %s", rec.Body)
		}
	}
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflow-runs/"+runs[4], nil).Body.String()
	if !strings.Contains(page, "checked-send") || !strings.Contains(page, "draft-send") {
		t.Fatal("missing answer history")
	}
	if sent := time.Date(2026, 11, 11, 20, 30, 0, 0, time.UTC).Local().Format("Jan 2, 2006 15:04:05 MST"); !strings.Contains(page, sent) {
		t.Fatalf("answer time not in the server's zone like the rest of the page, want %q", sent)
	}
}

func TestTaskRequestedAtUsesSubmissionTime(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Example task\nDate: Wed, 11 Nov 2026 14:39:00 -0500")
	code, r := sqlPost(t, h, q("SELECT abs(extract(epoch FROM (requested_at-created_at)))<5 FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != true {
		t.Fatalf("task timestamp: %+v", r)
	}
}

func TestRequestTimeMigrationBackfillsAndToleratesBadDates(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	emailDef := workflow.Starting
	emailDef.Trigger = workflow.TriggerEmail
	emailDef.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
	emailWF := createWorkflow(t, h, emailDef)
	voiceDef := workflow.Starting
	voiceDef.Trigger = workflow.TriggerVoiceMemo
	voiceDef.VoiceMemo = &workflow.VoiceMemoFilter{}
	voiceWF := createWorkflow(t, h, voiceDef)
	email := startRun(t, h, emailWF, "Example email\nDate: Wed, 11 Nov 2026 14:39:00 -0500 (EST)\n")
	voice := startRun(t, h, voiceWF, "Voice memo: Example\nRecorded: 2026-11-11T12:00:00Z\n")
	bad := startRun(t, h, wf, "Example email\nDate: invalid date\n")
	if _, err := d.Owner.Exec(t.Context(), `DROP VIEW workflow_runs_unanswered;
 DROP TABLE workflow_run_answers;
 ALTER TABLE workflow_runs DROP COLUMN requested_at;
 ALTER TABLE workflow_runs DROP COLUMN answer_expected;
 DELETE FROM schema_migrations WHERE version='0047_workflow_answers'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d.Owner); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, expected string }{
		{email, "TIMESTAMPTZ '2026-11-11T19:39:00Z'"},
		{voice, "TIMESTAMPTZ '2026-11-11T12:00:00Z'"},
		{bad, "created_at"},
	} {
		code, r := sqlPost(t, h, q("SELECT requested_at="+tc.expected+", answer_expected FROM workflow_runs WHERE id='"+tc.id+"'"))
		if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != true || r.Rows[0][1] != (tc.id != bad) {
			t.Fatalf("backfill: %d %+v", code, r)
		}
	}
}
