package web

import (
	"net/http"
	"testing"
)

func TestMemoryEstimateUsesRecentP90(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := namedWorkflow(t, h, "recent estimate", "/w/recent")
	// A single outlier in ten is excluded by discrete p90; 25% margin remains.
	pastRuns(t, d, wf, gib, gib, gib, gib, gib, gib, gib, gib, gib, 8*gib)
	check := func(n, bytes float64) {
		t.Helper()
		code, r := sqlPost(t, h, q(`SELECT observed_runs, estimated_memory_bytes FROM workflow_memory_estimate WHERE workflow_id='`+wf+`'`))
		if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != n || r.Rows[0][1] != bytes {
			t.Fatalf("estimate = %+v, want %v %v", r, n, bytes)
		}
	}
	check(10, float64(gib)*1.25)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET created_at=created_at-interval '8 days', finished_at=finished_at-interval '8 days' WHERE workflow_id=$1`, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET minute=minute-interval '8 days' WHERE agent_id LIKE $1`, "past-"+wf+"-%"); err != nil {
		t.Fatal(err)
	}
	id := startRun(t, h, wf, "no recent history")
	if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(4*gib) {
		t.Fatalf("stale history prevented fallback: %v", got)
	}
	// Move just one peak back into the window; its samples are still retained.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET finished_at=now()-interval '1 hour' WHERE id=(SELECT run_id FROM workflow_run_agents WHERE agent_id=$1)`, "past-"+wf+"-9"); err != nil {
		t.Fatal(err)
	}
	check(1, float64(gib)*10) // sparse history keeps its maximum, with margin
}
