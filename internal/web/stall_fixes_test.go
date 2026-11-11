package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRecoveryAttentionStates(t *testing.T) {
	f := newStallFixture(t)
	for _, state := range []string{"finished", "permission-parent", "permission-child", "pending", "error", "stuck", "cleared", "denied"} {
		t.Run(state, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			run := startRun(t, h, fixedWorkflow(t, h), "recover completed turns")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			rep := wfReport()
			rep.Paseo.Agents = []hostclient.Agent{
				{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"},
				{ID: "child", ParentID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
			blocked := state != "finished" && state != "cleared" && state != "denied"
			switch state {
			case "permission-parent":
				rep.Paseo.Agents[0].AttentionReason = "permission"
			case "permission-child":
				rep.Paseo.Agents[1].AttentionReason = "permission"
			case "pending", "error", "stuck", "cleared", "denied":
				var cleared any
				if state == "cleared" {
					cleared = clock.Now()
				}
				_, err := d.Owner.Exec(t.Context(), `INSERT INTO permission_resolutions(host,agent_id,permission_id,provider,mode,tool,status,message,attempted_at,received_at,cleared_at)
 VALUES('host-1','child','permission-1','codex','full-auto','shell',$1,'audit',$2,$2,$3)`, state, clock.Now(), cleared)
				if err != nil {
					t.Fatal(err)
				}
			}
			e := workflow.NewEngine(d.Owner, clock.Now)
			for i := 0; i < 2; i++ {
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				if err := e.ReconcileStalls(t.Context()); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					clock.Add(2 * time.Minute)
				}
			}
			var count int
			if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_continuations WHERE run_id=$1", run).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if blocked && count != 0 || !blocked && count != 1 {
				t.Fatalf("continuations=%d, permission blocked=%v", count, blocked)
			}
			if !blocked {
				succeed(t, b, b.command(), `{}`)
			}
			if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='stalled' WHERE id=$1", run); err != nil {
				t.Fatal(err)
			}
			rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/resume", `{}`, sameOriginFetch)
			want := 204
			if blocked {
				want = 409
			}
			if rec.Code != want {
				t.Fatalf("Resume=%d %s; want %d", rec.Code, rec.Body, want)
			}
		})
	}
}

func TestStallRecoveryWritesHistory(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	run := startRun(t, h, fixedWorkflow(t, h), "audit recovery")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle"}}
	e := workflow.NewEngine(d.Owner, clock.Now)
	for i, elapsed := range []time.Duration{0, 2 * time.Minute, 8 * time.Minute, 2 * time.Minute} {
		clock.Add(elapsed)
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := e.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		var updated time.Time
		if err := d.Owner.QueryRow(t.Context(), "SELECT updated_at FROM workflow_runs WHERE id=$1", run).Scan(&updated); err != nil {
			t.Fatal(err)
		}
		if !updated.Equal(clock.Now().Truncate(time.Microsecond)) {
			t.Fatalf("recovery did not update updated_at: %s vs %s", updated, clock.Now())
		}
		kind := "continuation"
		want := i
		if i == 3 {
			kind = "stalled"
			want = 1
		}
		var count int
		if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_events WHERE run_id=$1 AND kind=$2", run, kind).Scan(&count); err != nil || count != want {
			t.Fatalf("%s events=%d, want %d (%v)", kind, count, want, err)
		}
		if i < 3 {
			succeed(t, b, b.command(), `{}`)
		}
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	for _, text := range []string{"Continuation 1 of 2", "Continuation 2 of 2", "Stalled: orchestrator remained idle"} {
		if !strings.Contains(page, text) {
			t.Errorf("history missing %q", text)
		}
	}
}

func TestStalledPresentation(t *testing.T) {
	r := runRow{Status: workflow.Stalled, Step: 1, Steps: []workflow.Step{{Title: "Build"}, {Title: "Check"}, {Title: "Land"}}}
	if r.StatusClass() != "st-error" {
		t.Errorf("uncoloured stalled badge: %s", r.StatusClass())
	}
	if r.Where() != "Check" {
		t.Errorf("Now=%q", r.Where())
	}
	views := stepViews(r, []runEvent{{Kind: "next", Step: func() *int { n := 0; return &n }()}}, nil, t0)
	if views[0].State != "done" || views[1].State != "stalled" || views[1].Label != "stalled" || views[2].State != "pending" {
		t.Fatalf("step states: %+v", views)
	}
}
