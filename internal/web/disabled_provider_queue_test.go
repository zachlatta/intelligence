package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

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
					d := dbtest.New(t)
					clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
					h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
					b := sendReport(t, h, wfReport())
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
					e := workflow.NewEngine(d.Owner, clock.Now)
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
