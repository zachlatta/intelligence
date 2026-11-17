package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestEmailFollowUpContinuity(t *testing.T) {
	for _, mode := range []string{"three follow-ups", "agent gone", "older than 24 hours", "transport failure", "cleanup pending", "arrival during restoration"} {
		t.Run(mode, func(t *testing.T) {
			h, fake := emailServer(t)
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")
			b.captureCommands = true
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
				goneNow := mode == "agent gone" && i == count-1
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
				fake.Add(mailMsg(id, "thread", "christina@example.com", "Re: Question", "Did you do it?"))
				next := continuityDecided(t, h, id)["run_id"].(string)
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
					if cmd.Kind != "continue_email" || !strings.Contains(string(cmd.Args), "same-orchestrator") {
						t.Fatalf("continuation: %s %s", cmd.Kind, cmd.Args)
					}
					if goneNow || mode == "transport failure" {
						reason := "refresh_agent_request: Agent not found: same-orchestrator"
						if mode == "transport failure" {
							reason = "connection lost"
						}
						result := json.RawMessage(`{"agentGone":true}`)
						if mode == "transport failure" {
							result = nil
						}
						ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: reason, Result: result}})
						if ack.Error != "" {
							t.Fatal(ack.Error)
						}
						if mode == "transport failure" {
							eventually(t, "delivery error recorded", func() bool { return readRun(t, h, next)["status"] == "failed" })
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
							r := readRun(t, h, next)
							return r["orchestrator_id"] == "same-orchestrator" && r["pending_command"] == nil
						})
						_, events := sqlPost(t, h, q("SELECT continued_run_id::text FROM workflow_run_events WHERE run_id='"+next+"' AND kind='email_continued'"))
						if len(events.Rows) != 1 || events.Rows[0][0] != run {
							t.Fatalf("continuity events: %+v", events)
						}
					}
				}
				if goneNow || mode == "older than 24 hours" {
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
				_, rows := sqlPost(t, h, q("SELECT count(*),count(DISTINCT orchestrator_id),max(created_at)-min(created_at)<interval '1 hour' FROM workflow_runs"))
				if len(rows.Rows) != 1 || rows.Rows[0][0] != float64(4) || rows.Rows[0][1] != float64(1) || rows.Rows[0][2] != true {
					t.Fatalf("thread continuity: %+v", rows)
				}
			}
		})
	}
}
