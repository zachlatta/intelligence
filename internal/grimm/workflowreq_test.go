package grimm

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestParseCLIWorkflowCommands(t *testing.T) {
	t.Parallel()
	c, err := ParseCLI([]string{"ctl", "workflow", "request", "--workflow", "Deploy App", "--summary", "Ship v2", "-"}, strings.NewReader("Deploy release 2.0\n"))
	if err != nil {
		t.Fatalf("ParseCLI request: %v", err)
	}
	if c.Verb != "workflow" || len(c.Args) != 1 || c.Args[0] != "request" || c.Workflow != "Deploy App" || c.Summary != "Ship v2" || c.Text != "Deploy release 2.0" {
		t.Fatalf("got %+v", c)
	}

	c, err = ParseCLI([]string{"ctl", "workflow", "list"}, nil)
	if err != nil {
		t.Fatalf("ParseCLI list: %v", err)
	}
	if c.Verb != "workflow" || len(c.Args) != 1 || c.Args[0] != "list" {
		t.Fatalf("got %+v", c)
	}

	c, err = ParseCLI([]string{"ctl", "workflow", "status", "00000000-0000-4000-8000-000000000001"}, nil)
	if err != nil {
		t.Fatalf("ParseCLI status: %v", err)
	}
	if c.Verb != "workflow" || len(c.Args) != 2 || c.Args[0] != "status" || c.Args[1] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("got %+v", c)
	}

	if _, err := ParseCLI([]string{"ctl", "workflow", "request", "-"}, strings.NewReader("no workflow")); err == nil {
		t.Fatal("expected error when --workflow is missing")
	}
	if _, err := ParseCLI([]string{"ctl", "workflow", "request", "--workflow", "x"}, strings.NewReader("")); err == nil {
		t.Fatal("expected error when task input is empty")
	}
}

func TestWorkflowRequestApprovalAndSupervisorWakeups(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	wf := workflow.NewEngine(r.d.Owner, r.s.now)
	r.s.Workflows = wf

	// Save a workflow on host-1 with a step, a gate, and a signoff step.
	def := workflow.Starting
	def.Host = "host-1"
	def.Orchestrator.Provider = "antigravity"
	def.Orchestrator.Model = "gemini-4-neon-medium"
	def.Orchestrator.Thinking = ""
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{
		{ID: "build", Kind: workflow.KindStep, Title: "Build feature", Instructions: "Build it"},
		{ID: "verify", Kind: workflow.KindGate, Title: "Run tests", Instructions: "Verify it"},
		{ID: "signoff", Kind: workflow.KindSignoff, Title: "Zach sign-off", Instructions: "Ask Zach", RequireHumanSignoff: true},
	}
	wfID, err := wf.CreateWorkflow(ctx, r.acct, "Ship feature", "Ships a feature", def)
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	// Start a Grimm session with a live manager ("mgr") and one worker ("wrk-1").
	sid := r.ready()
	r.setAgent("mgr", "idle")

	w, err := r.s.HireWorker(ctx, r.acct, sid, Hire{Role: "builder", Name: "wren", Task: "build"})
	if err != nil {
		t.Fatalf("HireWorker: %v", err)
	}
	var wCmd string
	if err := r.d.Owner.QueryRow(ctx, `SELECT command_id::text FROM grimm_workers WHERE session_id = $1 AND name = $2`, sid, w.Name).Scan(&wCmd); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE commands SET status = 'succeeded', result = '{"agentId":"wrk-1"}' WHERE id = $1`, wCmd)
	r.reconcile(sid)

	// Worker cannot request a workflow run.
	_, err = r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: "Ship feature", Text: "Ship it",
	})
	if err == nil || !strings.Contains(err.Error(), "only the Grimm manager") {
		t.Fatalf("worker request err = %v, want 403 manager-only", err)
	}

	// Manager requests a workflow run by name.
	out, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: "Ship feature", Summary: "Run Ship feature", Text: "Build and ship widget X",
	})
	if err != nil {
		t.Fatalf("manager workflow request: %v", err)
	}
	if !strings.Contains(out, "pending") {
		t.Fatalf("unexpected request output: %s", out)
	}

	// Verify proposal was created as pending and NO workflow_runs row exists yet.
	var propID, propStatus string
	if err := r.d.Owner.QueryRow(ctx, `SELECT proposal_id::text, proposal_status FROM grimm_workflow_run_status WHERE session_id = $1`, sid).Scan(&propID, &propStatus); err != nil {
		t.Fatalf("query grimm_workflow_run_status: %v", err)
	}
	if propStatus != "pending" {
		t.Fatalf("proposal_status = %q, want pending", propStatus)
	}
	var runCount int
	if err := r.d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wfID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("runCount = %d, want 0 before approval", runCount)
	}

	// Verify thread item was created.
	var threadKind string
	if err := r.d.Owner.QueryRow(ctx, `SELECT kind FROM grimm_messages WHERE session_id = $1 AND channel = 'thread' AND kind = 'workflow_request'`, sid).Scan(&threadKind); err != nil {
		t.Fatalf("thread item missing: %v", err)
	}

	// List and status work.
	listOut, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "workflow", Args: []string{"list"}})
	if err != nil || !strings.Contains(listOut, "Ship feature") || !strings.Contains(listOut, "pending") {
		t.Fatalf("list = %q, err = %v", listOut, err)
	}
	statOut, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "workflow", Args: []string{"status", propID}})
	if err != nil || !strings.Contains(statOut, "Build and ship widget X") {
		t.Fatalf("status = %q, err = %v", statOut, err)
	}

	// Now approve the proposal (as Zach via workflow.Engine.Decide).
	if err := wf.Decide(ctx, r.acct, propID, "approve"); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	var runID string
	if err := r.d.Owner.QueryRow(ctx, `SELECT run_id::text FROM grimm_workflow_run_status WHERE proposal_id = $1`, propID).Scan(&runID); err != nil || runID == "" {
		t.Fatalf("run_id = %q, err = %v", runID, err)
	}

	// Reconcile picks up the approval/start and queues a wakeup.
	r.reconcile(sid)
	var wakeSummary string
	if err := r.d.Owner.QueryRow(ctx, `SELECT summary FROM grimm_wakeups WHERE session_id = $1 AND delivered_at IS NULL ORDER BY id DESC LIMIT 1`, sid).Scan(&wakeSummary); err != nil {
		t.Fatalf("expected wakeup after approval: %v", err)
	}
	if !strings.Contains(strings.ToLower(wakeSummary), "ship feature") {
		t.Fatalf("wakeSummary = %q", wakeSummary)
	}

	// Advance the run to step 1 (gate) and then waiting on Zach; reconcile wakes manager on each milestone.
	r.exec(`UPDATE workflow_runs SET status = 'running', step = 1, updated_at = now() WHERE id = $1`, runID)
	r.reconcile(sid)
	r.exec(`UPDATE workflow_runs SET status = 'waiting', step = 2, note = 'Ready to deploy? Please sign off.', updated_at = now() WHERE id = $1`, runID)
	r.reconcile(sid)

	// Advance clock past debounce and reconcile: manager receives one batched "[Grimm update]" message.
	r.later(updateDebounce + time.Second)
	r.reconcile(sid)
	var updateBody string
	if err := r.d.Owner.QueryRow(ctx, `SELECT body FROM grimm_messages WHERE session_id = $1 AND kind = 'update' ORDER BY id DESC LIMIT 1`, sid).Scan(&updateBody); err != nil {
		t.Fatalf("expected [Grimm update] thread row: %v", err)
	}
	if !strings.Contains(updateBody, "[Grimm update]") || !strings.Contains(updateBody, "Ready to deploy?") {
		t.Fatalf("updateBody = %q", updateBody)
	}
}
