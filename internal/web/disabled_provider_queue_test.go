package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// TestOrchestratorGetsSubscriptionPolicyOnce: the policy overrides saved model lists, so the
// orchestrator's first prompt carries it, once; with nothing disabled it carries none.
func TestOrchestratorGetsSubscriptionPolicyOnce(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "disabled", false: "all_enabled"}[disabled], func(t *testing.T) {
			h, d := dbServer(t)
			if !disabled {
				if _, err := d.Owner.Exec(t.Context(), `UPDATE provider_plan_settings SET enabled=true`); err != nil {
					t.Fatal(err)
				}
			}
			b := sendReport(t, h, wfReport())
			startRun(t, h, fixedWorkflow(t, h), "Policy run")
			var ws hostclient.CreateWorkspaceArgs
			if err := json.Unmarshal(b.command().Args, &ws); err != nil || ws.Agent == nil {
				t.Fatalf("orchestrator start: %v", err)
			}
			want := 0
			if disabled {
				want = 1
			}
			if n := strings.Count(ws.Agent.Prompt, "Subscription policy:"); n != want {
				t.Errorf("policy appears %d times, want %d", n, want)
			}
			if strings.HasSuffix(ws.Agent.Prompt, "\n\n") {
				t.Error("prompt ends with an empty policy")
			}
		})
	}
}

func TestDisabledOrchestratorCannotStrandProjectFinder(t *testing.T) {
	for _, provider := range []string{"muse", "antigravity", "gemini"} {
		for _, alreadyRunning := range []bool{false, true} {
			name := provider + "/before_launch"
			if alreadyRunning {
				name = provider + "/finder_running"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				h, d := dbServer(t)
				b := sendReport(t, h, wfReport())
				def := workflow.Starting
				def.Orchestrator.Provider = provider
				if alreadyRunning {
					if _, err := d.Owner.Exec(t.Context(), `UPDATE provider_plan_settings SET enabled=true WHERE provider=$1`, provider); err != nil {
						t.Fatal(err)
					}
				}
				run := startRun(t, h, createWorkflow(t, h, def), "Match a project")
				if alreadyRunning {
					finder := b.command()
					ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: finder.ID, Status: hostclient.CommandSucceeded, Result: []byte(`{"agentId":"finder-1"}`)}})
					if ack.Error != "" {
						t.Fatal(ack.Error)
					}
					if _, err := d.Owner.Exec(t.Context(), `UPDATE provider_plan_settings SET enabled=false WHERE provider=$1`, provider); err != nil {
						t.Fatal(err)
					}
					code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "project", "directory": "none"})
					if code != http.StatusOK || !strings.Contains(msg, "Stop") || !strings.Contains(msg, "provider "+provider+" is disabled") {
						t.Errorf("finder response: %d %s", code, msg)
					}
				}
				if status := runField(t, h, run, "status"); status != workflow.Failed {
					t.Errorf("status: %v", status)
				}
				if msg, _ := runField(t, h, run, "error").(string); !strings.Contains(msg, "provider "+provider+" is disabled") {
					t.Errorf("failure reason: %q", msg)
				}
				if runField(t, h, run, "finished_at") == nil {
					t.Error("missing completion time")
				}
				if runField(t, h, run, "pending_command") != nil {
					t.Error("startup command retained")
				}
				var starts, archives int
				if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM commands WHERE kind IN ('create_agent','create_workspace')`).Scan(&starts); err != nil {
					t.Fatal(err)
				}
				if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM commands WHERE kind='archive_agent' AND args->>'agentId'='finder-1'`).Scan(&archives); err != nil {
					t.Fatal(err)
				}
				want := 0
				if alreadyRunning {
					want = 1
				}
				if starts != want || archives != want {
					t.Errorf("starts=%d archives=%d, want %d", starts, archives, want)
				}
				// At 70% there is room for one run, provided the failed finder
				// no longer reserves capacity as a finding_project run.
				hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 70, 0.3)
				next := startRun(t, h, fixedWorkflow(t, h), "Next subscribed run")
				if status := runField(t, h, next, "status"); status != workflow.StartingOrchestrator {
					t.Errorf("failed finder retained capacity: next run %v", status)
				}
			})
		}
	}
}

func TestDisabledProviderDoesNotBlockHostQueue(t *testing.T) {
	for _, provider := range []string{"muse", "antigravity", "gemini"} {
		for _, finder := range []bool{false, true} {
			for _, retry := range []bool{false, true} {
				name := provider + "/orchestrator"
				if finder {
					name = provider + "/finder"
				}
				if retry {
					name += "/retry"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					d := dbtest.New(t)
					// Retry readiness uses host_status.online, which compares the
					// fake heartbeat with Postgres's real clock. Keep that heartbeat
					// fresh through the test's timeout and drive retries explicitly.
					clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute).Add(time.Hour)}
					h := newStallServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now })
					b := stallReport(t, h, wfReport())
					def := workflow.Starting
					def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
					if finder {
						def.Project = workflow.Starting.Project
						def.Project.Provider = provider
					} else {
						def.Orchestrator.Provider = provider
					}
					wf := createWorkflow(t, h, def)
					if retry {
						if _, err := d.Owner.Exec(t.Context(), `UPDATE provider_plan_settings SET enabled=true WHERE provider=$1`, provider); err != nil {
							t.Fatal(err)
						}
					} else {
						hostMinute(t, b, clock.Now().Add(-time.Minute), 97, 0.3)
					}
					first := startRun(t, h, wf, "Disabled provider run")
					if retry {
						original := b.command()
						hostMinute(t, b, clock.Now().Add(-time.Minute), 97, 0.3)
						ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: original.ID, Status: hostclient.CommandFailed, Error: "startup failed"}})
						if ack.Error != "" {
							t.Fatal(ack.Error)
						}
						if _, err := d.Owner.Exec(t.Context(), `UPDATE provider_plan_settings SET enabled=false WHERE provider=$1`, provider); err != nil {
							t.Fatal(err)
						}
					}
					clock.Add(time.Second)
					second := startRun(t, h, fixedWorkflow(t, h), "Subscribed provider run")
					clock.Add(time.Minute)
					e := h.local.wf
					if retry {
						if err := e.RetryStarts(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					hostMinute(t, b, clock.Now().Truncate(time.Minute).Add(-time.Minute), 20, 0.3)
					if err := e.StartQueued(t.Context()); err != nil {
						t.Fatal(err)
					}
					if status := runField(t, h, first, "status"); status != workflow.Failed {
						t.Errorf("disabled run: %v", status)
					}
					if msg, _ := runField(t, h, first, "error").(string); !strings.Contains(msg, "provider "+provider+" is disabled") {
						t.Errorf("failure reason: %q", msg)
					}
					if runField(t, h, first, "finished_at") == nil {
						t.Error("missing completion time")
					}
					if runField(t, h, first, "start_retry_at") != nil || runField(t, h, first, "start_retry_queued") != false {
						t.Error("retry state retained")
					}
					if status := runField(t, h, second, "status"); status != workflow.StartingOrchestrator {
						t.Errorf("later subscribed run: %v", status)
					}
					var queued, errors int
					if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_run_queue`).Scan(&queued); err != nil {
						t.Fatal(err)
					}
					if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_run_events WHERE run_id=$1 AND kind='error'`, first).Scan(&errors); err != nil {
						t.Fatal(err)
					}
					if queued != 0 || errors != 1 {
						t.Errorf("queue=%d error events=%d", queued, errors)
					}
				})
			}
		}
	}
}
