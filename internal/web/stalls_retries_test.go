package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStallWatchdogLeavesAdmissionAndStartupRetriesAlone(t *testing.T) {
	f := newStallFixture(t)
	for _, state := range []string{"queued", "finding_project", "starting", "retry_admission", "retry_backoff", "retry_running"} {
		t.Run(state, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			run := startRun(t, h, fixedWorkflow(t, h), "respect startup state")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			rep := wfReport()
			rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle"}}
			status := state
			var due any
			queued := false
			switch state {
			case "retry_admission":
				status = workflow.StartingOrchestrator
				due = clock.Now()
				queued = true
			case "retry_backoff":
				status = workflow.FindingProject
				due = clock.Now().Add(time.Hour)
			case "retry_running":
				status = workflow.Running
				due = clock.Now().Add(time.Hour)
			}
			if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status=$2,start_retry_at=$3,start_retry_queued=$4,queue_reason='waiting for admission' WHERE id=$1`, run, status, due, queued); err != nil {
				t.Fatal(err)
			}
			engine := workflow.NewEngine(d.Owner, clock.Now)
			for i := 0; i < 2; i++ {
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				if err := engine.ReconcileStalls(t.Context()); err != nil {
					t.Fatal(err)
				}
				clock.Add(2 * time.Minute)
			}
			var untouched bool
			if err := d.Owner.QueryRow(t.Context(), `SELECT status=$2 AND stall_checked_at IS NULL AND idle_since IS NULL
 AND queue_reason='waiting for admission' AND start_retry_queued=$3
 AND NOT EXISTS(SELECT 1 FROM workflow_run_continuations WHERE run_id=$1) FROM workflow_runs WHERE id=$1`, run, status, queued).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("watchdog changed startup/admission state: %v %v", untouched, err)
			}
		})
	}
}

func TestStallWatchdogContinuesOtherRunsAfterOneBadDefinition(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	badWorkflow := fixedWorkflow(t, h)
	bad := startRun(t, h, badWorkflow, "malformed saved definition")
	succeed(t, b, b.command(), `{"workspaceId":"w-bad","agentId":"orch-bad"}`)
	good := startRun(t, h, fixedWorkflow(t, h), "healthy saved definition")
	succeed(t, b, b.command(), `{"workspaceId":"w-good","agentId":"orch-good"}`)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_versions SET definition='[]'::jsonb WHERE workflow_id=$1`, badWorkflow); err != nil {
		t.Fatal(err)
	}
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-bad", WorkspaceID: "w-bad", Status: "idle"}, {ID: "orch-good", WorkspaceID: "w-good", Status: "idle"}}
	engine := workflow.NewEngine(d.Owner, clock.Now)
	for i := 0; i < 2; i++ {
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := engine.ReconcileStalls(t.Context()); err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("bad run missing from aggregate: %v", err)
		}
		clock.Add(2 * time.Minute)
	}
	var count int
	if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_continuations WHERE run_id=$1", good).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bad run starved healthy run: %d %v", count, err)
	}
}

func TestNewEngineReconcilesExistingIdleRun(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	run := startRun(t, h, fixedWorkflow(t, h), "resume after server restart")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle"}}
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if err := workflow.NewEngine(d.Owner, clock.Now).ReconcileStalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// A fresh instance finds the durable idle state without a subsequent host update.
	if err := workflow.NewEngine(dbtest.Pool(t, d), clock.Now).ReconcileStalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	command := b.command()
	if command.Kind != hostclient.SendMessageKind || !strings.Contains(string(command.Args), "current step") {
		t.Fatalf("startup recovery: %+v", command)
	}
	if r := readRun(t, h, run); r["status"] != workflow.Running || r["step"] != float64(0) {
		t.Fatalf("startup recovery moved run: %v", r)
	}
}
