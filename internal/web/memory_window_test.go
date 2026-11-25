package web

import (
	"testing"
)

func TestMemoryPercentileAndWorkflowFallback(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := namedWorkflow(t, h, "estimate", "/w/history")
	pastRuns(t, d, wf, 2*gib, 4*gib)
	id := startRun(t, h, wf, "sparse history")
	want := float64(3972844749) // ceil(3.7 GiB): continuous p85, no multiplier
	if got := runField(t, h, id, "estimated_memory_bytes"); got != want {
		t.Fatalf("estimate %v, want %v", got, want)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET repo='/w/other' WHERE status='succeeded'`); err != nil {
		t.Fatal(err)
	}
	id = startRun(t, h, wf, "workflow fallback")
	if got := runField(t, h, id, "estimated_memory_bytes"); got != want {
		t.Fatalf("workflow fallback %v, want %v", got, want)
	}
}

func TestOlderMemoryHistoryPrecedesUnknownFallback(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := namedWorkflow(t, h, "infrequent", "/w/infrequent")
	pastRuns(t, d, wf, 2*gib)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET created_at=created_at-interval '20 days',finished_at=finished_at-interval '20 days' WHERE workflow_id=$1`, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET minute=minute-interval '20 days' WHERE agent_id LIKE $1`, "past-"+wf+"-%"); err != nil {
		t.Fatal(err)
	}
	id := startRun(t, h, wf, "infrequent repo")
	if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(2*gib) {
		t.Fatalf("older history replaced by fallback: %v", got)
	}
}

func TestOversizedHeadStillAllowsLightBackfill(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/oversized")
	b.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(10*gib)); err != nil {
		t.Fatal(err)
	}
	repositoryMinute(t, b, 16<<30)
	head := startRun(t, h, wf, "cannot fit even alone")
	if got := runField(t, h, head, "status"); got != "queued" {
		t.Fatalf("unbounded solo start: %v", got)
	}
	light := startRun(t, h, namedWorkflow(t, h, "small", "/w/small"), "fits beside an oversized head")
	if got := runField(t, h, light, "status"); got != "starting" {
		t.Fatalf("oversized head held lighter work: %v", got)
	}
}
