package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRecoveryResolvesSavedModelTokensWithoutExpandingInput(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	rec := call(t, h, "POST", "/api/model-groups", `{"name":"Recovery Group","members":[]}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var group workflow.ModelGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	token := "{{" + group.Token + "}}"
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Steps[0].Instructions = "Build with " + token + ". Input {{input}}"
	run := startRun(t, h, createWorkflow(t, h, def), token)
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
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
	check := func() {
		t.Helper()
		c := b.command()
		var args hostclient.AgentArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(args.Text, "Build with no eligible model in "+group.Token+".") || !strings.Contains(args.Text, "Input "+token) || !strings.Contains(args.Text, "Do not launch a worker on this suggestion.") {
			t.Fatalf("unresolved or unsafe recovery instructions: %s", args.Text)
		}
		succeed(t, b, c, `{}`)
	}
	check()
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='stalled' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, "POST", "/api/workflow-runs/"+run+"/resume", `{}`, sameOriginFetch); rec.Code != 204 {
		t.Fatal(rec.Body)
	}
	check()
}

func TestStalledRunAcceptsValidMovesAndRejectsInvalidOnesAtomically(t *testing.T) {
	f := newStallFixture(t)
	for _, kind := range []string{"next", "skip", "back", "ask", "done", "fail", "summarize", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			b := f.reset(t)
			d, h := f.d, f.h
			key := keyOf(t, h, "host-1")
			run := startRun(t, h, fixedWorkflow(t, h), "continue after a stall")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			step := 0
			if kind == "back" {
				step = 1
			}
			if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='stalled',step=$2,error='idle recovery exhausted' WHERE id=$1`, run, step); err != nil {
				t.Fatal(err)
			}
			m := map[string]string{"move": kind, "summary": "a useful label", "why": "reason", "note": "question", "step": "build"}
			code, msg := move(t, h, key, run, m)
			wantCode := 200
			if kind == "invalid" {
				wantCode = 400
			}
			if code != wantCode {
				t.Fatalf("move %s=%d %s, want %d", kind, code, msg, wantCode)
			}
			wantStatus := workflow.Running
			wantStep := step
			wantError := ""
			wantResumes := 1
			switch kind {
			case "next", "skip":
				wantStep = 1
			case "back":
				wantStep = 0
			case "ask":
				wantStatus = workflow.Waiting
			case "done":
				wantStatus = workflow.Succeeded
			case "fail":
				wantStatus = workflow.Failed
				wantError = "reason"
			case "invalid":
				wantStatus = workflow.Stalled
				wantError = "idle recovery exhausted"
				wantResumes = 0
			}
			r := readRun(t, h, run)
			if r["status"] != wantStatus || r["step"] != float64(wantStep) || r["error"] != wantError {
				t.Fatalf("state=%v", r)
			}
			var resumes int
			if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_events WHERE run_id=$1 AND kind='resume'", run).Scan(&resumes); err != nil || resumes != wantResumes {
				t.Fatalf("resume events=%d, want %d (%v)", resumes, wantResumes, err)
			}
			var continuations int
			if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_continuations WHERE run_id=$1", run).Scan(&continuations); err != nil {
				t.Fatal(err)
			}
			if continuations != 0 {
				t.Fatal("valid activity queued an unnecessary continuation")
			}
		})
	}
}

func TestInputResumesStalledRunWithoutExtraWakeup(t *testing.T) {
	f := newStallFixture(t)
	for _, kind := range []string{"reply", "message"} {
		t.Run(kind, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			run := startRun(t, h, fixedWorkflow(t, h), "accept new input")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='stalled',error='idle recovery exhausted',idle_since=$2,idle_observation='old' WHERE id=$1`, run, clock.Now()); err != nil {
				t.Fatal(err)
			}
			if kind == "reply" {
				rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", `{"text":"Please continue"}`, sameOriginFetch)
				if rec.Code != 204 {
					t.Fatalf("reply=%d %s", rec.Code, rec.Body)
				}
			} else {
				ts := h
				e := workflow.NewEngine(d.Owner, clock.Now)
				if err := pgx.BeginFunc(t.Context(), d.Owner, func(tx pgx.Tx) error {
					return e.MessageTx(t.Context(), tx, ts.acct.ID, run, "A verified email reply", "Please continue")
				}); err != nil {
					t.Fatal(err)
				}
			}
			r := readRun(t, h, run)
			if r["status"] != workflow.Running || r["step"] != float64(0) || r["error"] != "" {
				t.Fatalf("input left run stalled: %v", r)
			}
			command := b.command()
			if command.Kind != hostclient.SendMessageKind || !strings.Contains(string(command.Args), "Please continue") || !strings.Contains(string(command.Args), "orch-1") {
				t.Fatalf("message: %+v", command)
			}
			var resumed, cleared bool
			if err := d.Owner.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM workflow_run_events WHERE run_id=$1 AND kind='resume'),idle_since IS NULL AND idle_observation='' FROM workflow_runs WHERE id=$1`, run).Scan(&resumed, &cleared); err != nil || !resumed || !cleared {
				t.Fatalf("recovery state: %v %v %v", resumed, cleared, err)
			}
			var messages int
			if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM commands WHERE kind='send_message'").Scan(&messages); err != nil || messages != 1 {
				t.Fatalf("messages=%d %v", messages, err)
			}
		})
	}
}

func TestInitializingWorkerBlocksRecoveryAndResume(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	run := startRun(t, h, fixedWorkflow(t, h), "wait for worker initialization")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}, {ID: "child", WorkspaceID: "w-run", ParentID: "orch-1", Status: "initializing"}}
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
	if count != 0 {
		t.Errorf("nudged while worker initialized: %d", count)
		succeed(t, b, b.command(), `{}`)
	}
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='stalled' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/resume", `{}`, sameOriginFetch)
	if rec.Code != 409 {
		t.Fatalf("Resume ignored initializing worker: %d %s", rec.Code, rec.Body)
	}
}

func TestContinuationWakeupsResetIdleClockButKeepAttemptBudget(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	start := clock.Now()
	run := startRun(t, h, fixedWorkflow(t, h), "measure idle windows")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
	e := workflow.NewEngine(d.Owner, clock.Now)
	observe := func(at time.Duration, status string) {
		t.Helper()
		clock.Add(start.Add(at).Sub(clock.Now()))
		a := &rep.Paseo.Agents[0]
		a.Status = status
		a.UpdatedAt = clock.Now()
		a.RequiresAttention = status == "idle"
		a.AttentionReason = ""
		if a.RequiresAttention {
			a.AttentionReason = "finished"
		}
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := e.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	// Keep unchanged idle snapshots unchanged: merely receiving a fresh report is not activity.
	check := func(at time.Duration) {
		t.Helper()
		clock.Add(start.Add(at).Sub(clock.Now()))
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := e.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_continuations WHERE run_id=$1", run).Scan(&n); err != nil || n != want {
			t.Fatalf("attempts=%d want %d (%v)", n, want, err)
		}
	}
	observe(0, "idle")
	check(2 * time.Minute)
	count(1)
	succeed(t, b, b.command(), `{}`)
	observe(2*time.Minute, "running")
	observe(3*time.Minute, "idle")
	check(12 * time.Minute)
	count(1)
	check(13 * time.Minute)
	count(2)
	succeed(t, b, b.command(), `{}`)
	observe(13*time.Minute, "running")
	observe(14*time.Minute, "idle")
	check(25 * time.Minute)
	count(2)
	if r := readRun(t, h, run); r["status"] != workflow.Running {
		t.Fatal(r)
	}
	check(26 * time.Minute)
	count(2)
	if r := readRun(t, h, run); r["status"] != workflow.Stalled {
		t.Fatal(r)
	}
}
