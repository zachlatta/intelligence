package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Full-message and thread reads are quota paced; allow scheduling slack under -race.
func continuityDecided(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if row := emailDecision(t, h, id); row != nil {
			return row
		}
	}
	t.Fatalf("timed out waiting for email %s", id)
	return nil
}

func readContinuityRun(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	code, result := sqlPost(t, h, q("SELECT * FROM workflow_runs WHERE id='"+id+"'"))
	if code != 200 || len(result.Rows) != 1 {
		t.Fatalf("continued run: %d %+v", code, result)
	}
	out := map[string]any{}
	for i, c := range result.Columns {
		out[c.Name] = result.Rows[0][i]
	}
	return out
}

func TestEmailFollowUpContinuity(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"three follow-ups", "agent gone", "older than 24 hours", "transport failure", "cleanup pending", "arrival during restoration", "pre-send inspect_agent", "pre-send inspect_workspace", "pre-send unrecoverable_workspace", "pre-send restore_workspace", "pre-send refresh_agent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h, fake := emailServer(t)
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")

			key := keyOf(t, h, "host-1")
			christina(t, h)
			connectGmail(t, h, fake)
			fake.Add(mailMsg("initial", "thread", "christina@example.com", "Question", "Please calculate."))
			run := continuityDecided(t, h, "initial")["run_id"].(string)
			succeed(t, b, b.command(), `{"workspaceId":"ws","agentId":"same-orchestrator"}`)
			count := 1
			if mode == "three follow-ups" || mode == "agent gone" {
				count = 3
			}
			for i := 0; i < count; i++ {
				beforeSendFailure := strings.HasPrefix(mode, "pre-send ")
				goneNow := mode == "agent gone" && i == count-1
				// Each request records its own checked answer. A retry records no second send.
				answerBody := fmt.Sprintf(`{"channel":"email","messageId":"answer-%d","threadId":"thread","checked":true}`, i)
				for range 2 {
					if rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", answerBody, map[string]string{"Authorization": "Bearer " + key}); rec.Code != 200 {
						t.Fatalf("answer: %d %s", rec.Code, rec.Body)
					}
				}
				if code, msg := move(t, h, key, run, map[string]string{"move": "done", "summary": "Earlier note: exact total $42.03"}); code != http.StatusOK {
					t.Fatalf("done: %d %s", code, msg)
				}
				cleanup := b.command()
				if mode != "cleanup pending" {
					succeed(t, b, cleanup, `{"workspaceId":"ws"}`)
				}
				if mode == "older than 24 hours" {
					if _, err := h.d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET finished_at=now()-interval '25 hours' WHERE id=$1", run); err != nil {
						t.Fatal(err)
					}
				}
				sent := mailMsg(fmt.Sprintf("sent%d", i), "thread", "bot@example.com", "Re: Question", "Exact answer $42.03\n\n> quoted history stays verbatim")
				sent.LabelIDs = []string{"SENT"}
				fake.Add(sent)
				id := fmt.Sprintf("follow%d", i)
				requested := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
				fake.Add(mailMsg(id, "thread", "christina@example.com", "Re: Question", "Did you do it?", email.Header{Name: "Date", Value: requested.Format(time.RFC1123Z)}))
				next := continuityDecided(t, h, id)["run_id"].(string)
				_, meta := sqlPost(t, h, q("SELECT r.answer_expected,r.requested_at=TIMESTAMPTZ '"+requested.Format(time.RFC3339)+"',r.project_selected,r.estimated_memory_bytes>0,r.priority=(SELECT priority FROM workflow_runs WHERE id='"+run+"'),s.answered_at IS NULL FROM workflow_runs r JOIN workflow_run_answer_status s ON s.id=r.id WHERE r.id='"+next+"'"))
				if len(meta.Rows) != 1 {
					t.Fatalf("metadata: %+v", meta)
				}
				for _, value := range meta.Rows[0] {
					if value != true {
						t.Fatalf("continued request must have independent answer state and current metadata: %+v", meta)
					}
				}
				if mode == "three follow-ups" {
					rec := call(t, anon(h), "POST", "/api/workflow-runs/"+next+"/answers", answerBody, map[string]string{"Authorization": "Bearer " + key})
					if rec.Code != http.StatusBadRequest {
						t.Fatalf("an earlier run's sent answer must not satisfy a follow-up: %d %s", rec.Code, rec.Body)
					}
					earlier := fmt.Sprintf(`{"channel":"email","messageId":"unrecorded-earlier-send","sentAt":%q,"checked":true}`, requested.Add(-time.Hour).Format(time.RFC3339))
					rec = call(t, anon(h), "POST", "/api/workflow-runs/"+next+"/answers", earlier, map[string]string{"Authorization": "Bearer " + key})
					if rec.Code != http.StatusBadRequest {
						t.Fatalf("pre-request answer accepted: %d %s", rec.Code, rec.Body)
					}
				}
				if mode == "cleanup pending" {
					_, queued := sqlPost(t, h, q("SELECT status FROM commands WHERE kind='continue_email'"))
					if len(queued.Rows) != 1 || queued.Rows[0][0] != "queued" {
						t.Fatalf("cleanup ordering: %+v", queued)
					}
					succeed(t, b, cleanup, `{"workspaceId":"ws"}`)
				}
				cmd := b.command()
				if mode == "arrival during restoration" {
					fake.Add(mailMsg("extra", "thread", "christina@example.com", "Re: Question", "And show the calculation."))
					if got := continuityDecided(t, h, "extra"); got["run_id"] != next || got["decision"] != "added" {
						t.Fatalf("additional email: %v", got)
					}
				}
				if mode == "older than 24 hours" {
					if cmd.Kind != hostclient.CreateWorkspaceKind {
						t.Fatalf("old follow-up: %s", cmd.Kind)
					}
				} else {
					if cmd.Kind != "continue_email" || !strings.Contains(string(cmd.Args), "same-orchestrator") || strings.Count(string(cmd.Args), "Correction:") == 0 {
						t.Fatalf("continuation: %s %s", cmd.Kind, cmd.Args)
					}
					if goneNow || beforeSendFailure || mode == "transport failure" {
						reason := "refresh_agent_request: Agent not found: same-orchestrator"
						if mode == "transport failure" {
							reason = "connection lost"
						}
						result := json.RawMessage(`{"agentGone":true}`)
						if beforeSendFailure {
							result, _ = json.Marshal(map[string]any{"notDelivered": true, "failureStage": strings.TrimPrefix(mode, "pre-send ")})
							reason = "Cannot revive before sending: " + mode
						}
						if mode == "transport failure" {
							result = nil
						}
						ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: reason, Result: result}})
						if ack.Error != "" {
							t.Fatal(ack.Error)
						}
						if mode == "transport failure" {
							eventually(t, "delivery error recorded", func() bool { return readContinuityRun(t, h, next)["status"] == "failed" })
							return
						}
						cmd = b.command()
						if cmd.Kind != hostclient.CreateWorkspaceKind {
							t.Fatalf("fallback: %s", cmd.Kind)
						}
					} else {
						succeed(t, b, cmd, `{"workspaceId":"ws","agentId":"same-orchestrator"}`)
						if mode == "arrival during restoration" {
							msg := b.command()
							if msg.Kind != hostclient.SendMessageKind || !strings.Contains(string(msg.Args), "And show the calculation.") {
								t.Fatalf("additional message: %+v", msg)
							}
						}
						eventually(t, "same orchestrator", func() bool {
							r := readContinuityRun(t, h, next)
							return r["orchestrator_id"] == "same-orchestrator" && r["pending_command"] == nil
						})
						_, events := sqlPost(t, h, q("SELECT continued_run_id::text FROM workflow_run_events WHERE run_id='"+next+"' AND kind='email_continued'"))
						if len(events.Rows) != 1 || events.Rows[0][0] != run {
							t.Fatalf("continuity events: %+v", events)
						}
						// Event text is a short, linked line for the run page; the input lives on the run.
						_, lines := sqlPost(t, h, q("SELECT text FROM workflow_run_events WHERE run_id='"+next+"' AND kind IN ('email_continuation_requested','email_continued') ORDER BY id"))
						for _, row := range lines.Rows {
							if text := fmt.Sprint(row[0]); !strings.Contains(text, run) || strings.Contains(text, "Earlier sent answers") {
								t.Fatalf("continuity event text: %q", text)
							}
						}
					}
				}
				if goneNow || beforeSendFailure || mode == "older than 24 hours" {
					_, coldEvent := sqlPost(t, h, q("SELECT continued_run_id::text FROM workflow_run_events WHERE run_id='"+next+"' AND kind='email_cold_start'"))
					if len(coldEvent.Rows) != 1 || coldEvent.Rows[0][0] != run {
						t.Fatalf("cold-start predecessor: %+v", coldEvent)
					}
					_, coldText := sqlPost(t, h, q("SELECT text FROM workflow_run_events WHERE run_id='"+next+"' AND kind='email_cold_start'"))
					if len(coldText.Rows) != 1 || !strings.Contains(fmt.Sprint(coldText.Rows[0][0]), run) {
						t.Fatalf("cold-start text names no earlier run: %+v", coldText)
					}
					var a hostclient.CreateWorkspaceArgs
					if err := json.Unmarshal(cmd.Args, &a); err != nil {
						t.Fatal(err)
					}
					for _, want := range []string{"Exact answer $42.03\n\n> quoted history stays verbatim", "Earlier note: exact total $42.03", "Correction:", "Nothing was changed", "yes/no"} {
						if a.Agent == nil || !strings.Contains(a.Agent.Prompt, want) {
							t.Errorf("fallback missing %q: %s", want, cmd.Args)
						}
					}
					if goneNow {
						for n := 0; n < count; n++ {
							if !strings.Contains(a.Agent.Prompt, fmt.Sprintf("Sent message sent%d", n)) {
								t.Errorf("missing earlier sent answer %d", n)
							}
						}
					}
					return
				}
				run = next
			}
			if mode == "three follow-ups" {
				rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", `{"channel":"email","messageId":"answer-final","threadId":"thread","checked":true}`, map[string]string{"Authorization": "Bearer " + key})
				if rec.Code != 200 {
					t.Fatalf("final answer: %d %s", rec.Code, rec.Body)
				}
				if code, msg := move(t, h, key, run, map[string]string{"move": "done", "summary": "Final follow-up answered"}); code != 200 {
					t.Fatalf("final completion: %d %s", code, msg)
				}
				succeed(t, b, b.command(), `{"workspaceId":"ws"}`)
				_, rows := sqlPost(t, h, q("SELECT count(*),count(DISTINCT orchestrator_id),max(created_at)-min(created_at)<interval '1 hour' FROM workflow_runs"))
				if len(rows.Rows) != 1 || rows.Rows[0][0] != float64(4) || rows.Rows[0][1] != float64(1) || rows.Rows[0][2] != true {
					t.Fatalf("thread continuity: %+v", rows)
				}
				_, answers := sqlPost(t, h, q("SELECT count(*),count(DISTINCT run_id) FROM workflow_run_answers"))
				if len(answers.Rows) != 1 || answers.Rows[0][0] != float64(4) || answers.Rows[0][1] != float64(4) {
					t.Fatalf("duplicate or shared answers: %+v", answers)
				}
			}
		})
	}
}

func TestEmailContinuityEventsReadNaturally(t *testing.T) {
	t.Parallel()
	const earlier = "11111111-2222-3333-4444-555555555555"
	for _, kind := range []string{"email_continuation_requested", "email_continued", "email_cold_start"} {
		got := (runEvent{Kind: kind, Text: "Follow-up to run " + earlier}).Line()
		if strings.Contains(got, kind) || !strings.Contains(got, "(/workflow-runs/"+earlier+")") {
			t.Errorf("%s reads %q", kind, got)
		}
	}
}
