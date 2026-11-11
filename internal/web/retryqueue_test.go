package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStartRetrySharesCapacityQueue(t *testing.T) {
	for _, phase := range []string{workflow.FindingProject, workflow.StartingOrchestrator} {
		t.Run(phase, func(t *testing.T) {
			d := dbtest.New(t)
			clock := &fakeClock{t: time.Now().UTC()}
			h := newServer(t, d, d.Owner, "v", func(c *Config) {
				c.Now = clock.Now
				c.QueueInterval = time.Hour
			})
			b := sendReport(t, h, wfReport())
			def := workflow.Starting
			if phase == workflow.StartingOrchestrator {
				def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			}
			wf := createWorkflow(t, h, def)
			run := startRun(t, h, wf, "First run")
			original := b.command()
			fail := func(id string) {
				t.Helper()
				ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{
					ID: id, Status: hostclient.CommandFailed, Error: "temporary startup error",
				}})
				if ack.Error != "" {
					t.Fatal(ack.Error)
				}
			}
			hostMinute(t, b, clock.Now().Truncate(time.Minute), 95, 0.3)
			fail(original.ID)
			clock.Add(time.Second)
			second := startRun(t, h, wf, "Second run")
			clock.Add(15 * time.Second)
			e := workflow.NewEngine(d.Owner, clock.Now)
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if status := runField(t, h, run, "status"); status != phase {
				t.Fatalf("capacity wait should preserve startup phase, got %v", status)
			}
			if attempts := runField(t, h, run, "start_retries"); attempts != float64(0) {
				t.Fatalf("capacity wait spent a retry: %v", attempts)
			}
			page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
			if !strings.Contains(page, "Retry 1 of 3 queued") || !strings.Contains(page, "1st in line") || !strings.Contains(page, "CPU 95%") {
				t.Fatal("page should show retry progress and capacity queue position")
			}

			// One run fits at 70%. The older retry retains its FIFO place, and
			// the reservation should account for exactly one run.
			clock.Add(time.Minute)
			hostMinute(t, b, clock.Now().Truncate(time.Minute), 70, 0.3)
			launch, _ := runField(t, h, run, "launch_command").(string)
			if launch == "" || launch == original.ID || launch != runField(t, h, run, "pending_command") {
				t.Fatalf("retry reservation points at the wrong attempt: %v", launch)
			}
			if status := runField(t, h, run, "status"); status != phase {
				t.Fatalf("retry should resume %s, got %v", phase, status)
			}
			var key string
			if err := d.Owner.QueryRow(t.Context(), "SELECT idempotency_key FROM commands WHERE id=$1", launch).Scan(&key); err != nil || key != original.ID {
				t.Fatalf("retry key = %q, %v", key, err)
			}
			reason, _ := runField(t, h, second, "queue_reason").(string)
			if status := runField(t, h, second, "status"); status != workflow.Queued || !strings.Contains(reason, "90% with 1 run starting") {
				t.Fatalf("reservation should hold one slot, status %v, reason %q", status, reason)
			}

			// The failed attempt releases its slot immediately, even within
			// the same measurement window. The next backoff uses no capacity.
			fail(launch)
			if status := runField(t, h, second, "status"); status != phase {
				t.Fatalf("failed retry should release capacity for the next run, got %v", status)
			}
		})
	}
}
