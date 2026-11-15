package web

import (
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestExpectedAnswersRequireCheckedSendsAndExcludeCanceledAndTasks(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	for _, trigger := range []string{workflow.TriggerTask, workflow.TriggerEmail, workflow.TriggerVoiceMemo} {
		def := workflow.Starting
		def.Trigger = trigger
		if trigger == workflow.TriggerEmail {
			def.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
		}
		if trigger == workflow.TriggerVoiceMemo {
			def.VoiceMemo = &workflow.VoiceMemoFilter{}
		}
		wf := createWorkflow(t, h, def)
		for _, status := range []string{"queued", "starting", "succeeded", "failed", "canceled"} {
			run := startRun(t, h, wf, "Example request")
			_, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET requested_at=now()-interval '1 hour', status=$2,
    finished_at=CASE WHEN $2 IN ('succeeded','failed','canceled') THEN now() END WHERE id=$1`, run, status)
			if err != nil {
				t.Fatal(err)
			}
			code, r := sqlPost(t, h, q("SELECT answer_expected FROM workflow_runs WHERE id='"+run+"'"))
			expected := trigger != workflow.TriggerTask
			if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != expected {
				t.Fatalf("expectation %s: %+v", trigger, r)
			}
			overdue := expected && status != "canceled"
			checkView := func(unchecked bool) {
				t.Helper()
				code, r := sqlPost(t, h, q("SELECT has_unchecked_sends FROM workflow_runs_unanswered WHERE id='"+run+"'"))
				n := 0
				if overdue {
					n = 1
				}
				if code != 200 || len(r.Rows) != n || (n == 1 && r.Rows[0][0] != unchecked) {
					t.Fatalf("view %s/%s: %+v", trigger, status, r)
				}
			}
			checkView(false)
			page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
			terminal := status == "succeeded" || status == "failed"
			if strings.Contains(page, "No answer recorded") != (expected && terminal) {
				t.Fatalf("unexpected missing-answer flag for %s/%s", trigger, status)
			}

			post := func(body string) {
				t.Helper()
				rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", body, map[string]string{"Authorization": "Bearer " + key})
				if rec.Code != 200 {
					t.Fatalf("answer: %s", rec.Body)
				}
			}
			// Omitted send time is server time; retries must preserve the original receipt time.
			body := `{"channel":"email","messageId":"unchecked-example"}`
			post(body)
			post(body)
			code, r = sqlPost(t, h, q("SELECT count(*),bool_and(abs(extract(epoch FROM (sent_at-now())))<30) FROM workflow_run_answers WHERE run_id='"+run+"'"))
			if code != 200 || len(r.Rows) != 1 || r.Rows[0][1] != true {
				t.Fatalf("server time: %+v", r)
			}
			checkView(true)
			for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
				page := get(t, h, path, nil).Body.String()
				if !strings.Contains(page, "Only unchecked sends") {
					t.Fatalf("unchecked send not visible on %s", path)
				}
			}
			post(`{"channel":"email","messageId":"checked-example","sentAt":"2026-11-12T20:00:00Z","checked":true}`)
			overdue = false
			checkView(true)
		}
	}
}

func TestAnswerTimingUsesCheckedAnswer(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Example task")
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET requested_at='2026-11-12T12:00:00Z' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"channel":"email","messageId":"first","sentAt":"2026-11-12T12:06:00Z"}`,
		`{"channel":"email","messageId":"reviewed","sentAt":"2026-11-12T12:15:00Z","checked":true}`,
	} {
		rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", body, map[string]string{"Authorization": "Bearer " + key})
		if rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, "answered: 15.0 min") || !strings.Contains(page, "Unchecked send at 6.0 min") {
			t.Fatalf("incorrect timing on %s", path)
		}
	}
}
