package web

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStartExpiryReconcilesWithoutAHostUpdate(t *testing.T) {
	h, db := dbServer(t)
	b := sendReport(t, h, wfReport())
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "expired")
	c := b.command()
	// Fleet expiry has no host command_update frame. Reconciliation must find it.
	if _, err := db.Owner.Exec(t.Context(), "UPDATE commands SET status='expired', error='expired with no result from the host: it may or may not have run' WHERE id=$1", c.ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	e := workflow.NewEngine(db.Owner, func() time.Time { return at })
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
		t.Fatal(err)
	}
	e = workflow.NewEngine(db.Owner, func() time.Time { return due })
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := b.command()
	if next.IdempotencyKey != c.ID {
		t.Fatal(next)
	}
	succeed(t, b, next, `{"agentId":"finder"}`)
	if r := readRun(t, h, run); r["project_agent_id"] != "finder" || r["error"] != "" {
		t.Fatal(r)
	}
	var retries int
	if err := db.Owner.QueryRow(t.Context(), "SELECT start_retries FROM workflow_runs WHERE id=$1", run).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 0 {
		t.Fatalf("finder did not reset budget: %d", retries)
	}
}
