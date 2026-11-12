package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStallRecoveryIsBoundedAndResumable(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, h, createWorkflow(t, h, def), "recover safely")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	engine := workflow.NewEngine(d.Owner, clock.Now)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}, {ID: "child", WorkspaceID: "w-run", ParentID: "orch-1", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
	reconcile := func() {
		t.Helper()
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := engine.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		code, r := sqlPost(t, h, q("SELECT count(*) FROM workflow_run_continuations"))
		if code != 200 || r.Rows[0][0] != float64(want) {
			t.Fatalf("continuations: %d %+v", code, r)
		}
	}
	reconcile()
	count(0)
	clock.Add(2 * time.Minute)
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, e := range []*workflow.Engine{engine, workflow.NewEngine(dbtest.Pool(t, d), clock.Now)} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- e.ReconcileStalls(t.Context()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count(1)
	c := b.command()
	if c.Kind != "send_message" || !strings.Contains(string(c.Args), fmt.Sprintf("Step 1 of %d", len(def.Steps))) {
		t.Fatalf("continuation: %+v", c)
	}
	succeed(t, b, c, `{}`)
	// A second instance and a restart cannot duplicate the first attempt.
	other := workflow.NewEngine(dbtest.Pool(t, d), clock.Now)
	if err := other.ReconcileStalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	count(1)
	clock.Add(8 * time.Minute)
	reconcile()
	count(2)
	succeed(t, b, b.command(), `{}`)
	// Main can update a label independently of the lifecycle. It must not reset recovery.
	if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "summarize", "summary": "A clearer label"}); code != 200 {
		t.Fatalf("summarize: %d %s", code, msg)
	}
	clock.Add(2 * time.Minute)
	reconcile()
	count(2)
	if r := readRun(t, h, run); r["status"] != "stalled" || r["error"] == "" {
		t.Fatalf("run: %v", r)
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if !strings.Contains(page, "Stalled") || !strings.Contains(page, `id="resume"`) {
		t.Fatal("no stalled Resume UI")
	}
	path := "/api/workflow-runs/" + run + "/resume"
	bob, err := h.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if r := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch, sessionFor(t, h.store, bob)); r.Code != 404 {
		t.Fatalf("other account: %d", r.Code)
	}
	if r := call(t, h, http.MethodPost, path, `{}`, map[string]string{"Origin": "https://other.example"}); r.Code != 403 {
		t.Fatalf("cross origin: %d", r.Code)
	}
	if r := call(t, h, http.MethodPost, path, `{"extra":true}`, sameOriginFetch); r.Code != 400 {
		t.Fatalf("bad resume body: %d", r.Code)
	}

	if r := call(t, anon(h), http.MethodPost, path, `{}`, sameOriginFetch); r.Code != 401 {
		t.Fatalf("anonymous resume: %d", r.Code)
	}
	if r := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch); r.Code != 204 {
		t.Fatalf("resume: %d %s", r.Code, r.Body)
	}
	count(3)
	succeed(t, b, b.command(), `{}`)
	if r := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch); r.Code != 409 {
		t.Fatalf("double resume: %d", r.Code)
	}
	if r := readRun(t, h, run); r["status"] != "running" || r["step"] != float64(0) {
		t.Fatalf("resume moved run: %v", r)
	}
	if rec := call(t, h, http.MethodDelete, "/api/hosts/host-1", "", sameOriginFetch); rec.Code != 204 {
		t.Fatalf("remove recovered host: %d %s", rec.Code, rec.Body)
	}
	if err := db.CheckContract(context.Background(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogExcludesUnsafeContinuations(t *testing.T) {
	f := newStallFixture(t)
	for _, why := range []string{"worker", "permission", "pending", "waiting", "offline", "stale", "canceled", "missing", "busy"} {
		t.Run(why, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			def := workflow.Starting
			def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			run := startRun(t, h, createWorkflow(t, h, def), "safe recovery")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			engine := workflow.NewEngine(d.Owner, clock.Now)
			rep := wfReport()
			rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle"}}
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			if err := engine.ReconcileStalls(t.Context()); err != nil {
				t.Fatal(err)
			}
			clock.Add(3 * time.Minute)
			switch why {
			case "worker":
				rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "child", ParentID: "orch-1", WorkspaceID: "other-workspace", Status: "running"})
			case "permission":
				rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "child", ParentID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "permission"})
			case "missing":
				rep.Paseo.Agents = nil
			case "busy":
				rep.Paseo.Agents[0].Status = "running"
			}
			if why != "stale" {
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
			}
			switch why {
			case "pending":
				call(t, h, http.MethodPost, "/api/hosts/host-1/commands", `{"kind":"send_message","args":{"agentId":"orch-1","text":"reply"}}`, sameOriginFetch)
			case "waiting", "canceled":
				if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status=$2 WHERE id=$1", run, why); err != nil {
					t.Fatal(err)
				}
			case "offline":
				if _, err := d.Owner.Exec(t.Context(), "UPDATE hosts SET stream_id=NULL WHERE name='host-1'"); err != nil {
					t.Fatal(err)
				}
			}
			if err := engine.ReconcileStalls(t.Context()); err != nil {
				t.Fatal(err)
			}
			code, r := sqlPost(t, h, q("SELECT count(*) FROM workflow_run_continuations"))
			if code != 200 || r.Rows[0][0] != float64(0) {
				t.Fatalf("unsafe continuation: %+v", r)
			}
			// A finished child gets the full grace period, then the existing parent resumes.
			if why == "worker" {
				rep.Paseo.Agents[1].Status = "idle"
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				if err := engine.ReconcileStalls(t.Context()); err != nil {
					t.Fatal(err)
				}
				clock.Add(2 * time.Minute)
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				if err := engine.ReconcileStalls(t.Context()); err != nil {
					t.Fatal(err)
				}
				_, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_continuations"))
				if r.Rows[0][0] != float64(1) {
					t.Fatal("did not resume after child completion grace")
				}
			}
		})
	}
}

func TestContinuationIsNotDeliveredAfterProgress(t *testing.T) {
	f := newStallFixture(t)
	for _, ending := range []string{"next", "ask", "fail", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			def := workflow.Starting
			def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			run := startRun(t, h, createWorkflow(t, h, def), "invalidate continuation")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			// The test controls exactly when this continuation is claimed.
			var command string
			err := d.Owner.QueryRow(t.Context(), `INSERT INTO commands(host,kind,args,created_at,expires_at) VALUES('host-1','send_message','{"agentId":"orch-1","text":"continue"}',$1,$1::timestamptz+interval '1 minute') RETURNING id::text`, clock.Now()).Scan(&command)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_continuations(run_id,account_id,event_id,attempt,command_id,created_at) SELECT id,account_id,(SELECT max(id) FROM workflow_run_events WHERE run_id=$1),1,$2,$3 FROM workflow_runs WHERE id=$1`, run, command, clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ending == "cancel" {
				if r := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); r.Code != 204 {
					t.Fatal(r.Code)
				}
			} else {
				if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": ending, "why": "reason", "note": "question"}); code != 200 {
					t.Fatalf("move: %d %s", code, msg)
				}
			}
			claimed, err := fleet.New(d.Owner).Claim(t.Context(), "host-1", fleet.ClaimOptions{Replay: true}, clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range claimed {
				if c.ID == command {
					t.Fatal("stale continuation delivered")
				}
			}
		})
	}
}

// Admission landed after this worktree branched; recovery must preserve its queued status.
func TestStallMigrationPreservesAdmissionState(t *testing.T) {
	f := newStallFixture(t)
	f.reset(t)
	h, d := f.h, f.d
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, h, createWorkflow(t, h, def), "preserve admission")
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='queued' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewEngine(d.Owner, f.clock.Now).ReconcileStalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := readRun(t, h, run); r["status"] != "queued" {
		t.Fatal("watchdog changed admission state")
	}
}
