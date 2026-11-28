package grimm

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestPromptsWriteAndAutonomyContract(t *testing.T) {
	t.Parallel()
	sess := Session{ID: "11111111-1111-4111-8111-111111111111", Host: "host-1", Repo: "/w/desk", Branch: "grimm/11111111", MaxWorkers: 10}
	mp := managerPrompt(sess, []string{SeedDirective}, []string{"Fix a bug in app"})
	for _, want := range []string{
		"Research & Analysis",
		"Small Prototypes & Explorations",
		"Workflow-required",
		"Human-approval semantics",
		"Factual pre-flight",
		"workflow catalog",
		"workflow request",
		"workflow withdraw",
		"workflow events",
		"workflow peek",
		"workflow input",
		"workflow reply",
		"workflow cancel",
		"Repo:",
	} {
		if !strings.Contains(mp, want) {
			t.Errorf("managerPrompt missing %q", want)
		}
	}
	if strings.Contains(mp, "hire at least two workers in the same message so they run in parallel on different plans: a builder, and an independent verifier") {
		t.Error("managerPrompt still contains old builder+verifier default")
	}
	if strings.Contains(mp, "host-1 workflow") {
		t.Error("managerPrompt has broken format-verb substitution")
	}

	wp := workerPrompt(sess, "athena", "researcher", "Read repo", []string{"athena"}, []string{SeedDirective})
	for _, want := range []string{
		"read-only for production repositories",
		"workflow catalog|list|status|events|peek",
		"intelligence workflow next/back/skip/done/fail/steer",
	} {
		if !strings.Contains(wp, want) {
			t.Errorf("workerPrompt missing %q", want)
		}
	}

	for _, role := range []string{"researcher", "analyst", "prototyper"} {
		if PolicyFor(role).Reason == "" {
			t.Errorf("PolicyFor(%q) missing reason", role)
		}
	}
	if PolicyFor("reviewer").Pinned[OpusProvider] != OpusModel {
		t.Errorf("reviewer policy not pinned to Opus 5.5")
	}
}

func TestParseCLILargeWorkflowInputAndNewVerbs(t *testing.T) {
	t.Parallel()
	// Regression: stdin > MaxText (20 KB) and > 64 KiB must NOT be truncated for workflow verbs.
	big := strings.Repeat("A", 100*1024)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"request", []string{"ctl", "workflow", "request", "--workflow", "Factory Task", "--replaces", "prop-1", "--repo", "/w/app", "-"}},
		{"input", []string{"ctl", "workflow", "input", "run-1", "-"}},
		{"reply", []string{"ctl", "workflow", "reply", "run-1", "-"}},
	} {
		c, err := ParseCLI(tc.args, strings.NewReader(big))
		if err != nil {
			t.Fatalf("%s: ParseCLI: %v", tc.name, err)
		}
		if len(c.Text) != len(big) {
			t.Fatalf("%s: len(Text) = %d, want %d (truncated!)", tc.name, len(c.Text), len(big))
		}
		if tc.name == "request" && (c.Replaces != "prop-1" || c.Repo != "/w/app") {
			t.Fatalf("request flags lost: %+v", c)
		}
	}

	// Catalog, withdraw, events, peek, cancel
	if c, err := ParseCLI([]string{"ctl", "workflow", "catalog"}, nil); err != nil || c.Args[0] != "catalog" {
		t.Fatalf("catalog: %+v %v", c, err)
	}
	if c, err := ParseCLI([]string{"ctl", "workflow", "withdraw", "p-1", "--reason", "superseded"}, nil); err != nil || c.Reason != "superseded" || c.Args[1] != "p-1" {
		t.Fatalf("withdraw: %+v %v", c, err)
	}
	if c, err := ParseCLI([]string{"ctl", "workflow", "events", "r-1", "--since", "42", "--limit", "25"}, nil); err != nil || c.Since != 42 || c.Lines != 25 {
		t.Fatalf("events: %+v %v", c, err)
	}
	if c, err := ParseCLI([]string{"ctl", "workflow", "peek", "r-1", "--agent", "orchestrator", "--lines", "15"}, nil); err != nil || c.Agent != "orchestrator" || c.Lines != 15 {
		t.Fatalf("peek: %+v %v", c, err)
	}
	if c, err := ParseCLI([]string{"ctl", "workflow", "cancel", "r-1", "--why", "human asked"}, nil); err != nil || c.Reason != "human asked" {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if _, err := ParseCLI([]string{"ctl", "workflow", "withdraw", "p-1"}, nil); err == nil {
		t.Fatal("withdraw without --reason should fail")
	}
	if _, err := ParseCLI([]string{"ctl", "workflow", "cancel", "r-1"}, nil); err == nil {
		t.Fatal("cancel without --why should fail")
	}
}

func TestWorkflowOrchestrationVerbsGuardsAndRateLimit(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	wf := workflow.NewEngine(r.d.Owner, r.s.now)
	r.s.Workflows = wf

	def := workflow.Starting
	def.Host = "host-1"
	def.Orchestrator.Provider = "antigravity"
	def.Orchestrator.Model = "gemini-4-neon-medium"
	def.Orchestrator.Thinking = ""
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{
		{ID: "build", Kind: workflow.KindStep, Title: "Build", Instructions: "Build it"},
		{ID: "signoff", Kind: workflow.KindSignoff, Title: "Human sign-off", Instructions: "Ask for sign-off", RequireHumanSignoff: true},
	}
	wfID, err := wf.CreateWorkflow(ctx, r.acct, "Factory Task", "End-to-end build workflow", def)
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	sid := r.ready()
	r.setAgent("mgr", "idle")
	w, err := r.s.HireWorker(ctx, r.acct, sid, Hire{Role: "researcher", Name: "scout", Task: "research"})
	if err != nil {
		t.Fatalf("HireWorker: %v", err)
	}
	var wCmd string
	_ = r.d.Owner.QueryRow(ctx, `SELECT command_id::text FROM grimm_workers WHERE session_id = $1 AND name = $2`, sid, w.Name).Scan(&wCmd)
	r.exec(`UPDATE commands SET status = 'succeeded', result = '{"agentId":"wrk-1"}' WHERE id = $1`, wCmd)
	r.reconcile(sid)

	// Catalog works for both manager and worker.
	catMgr, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "workflow", Args: []string{"catalog"}})
	if err != nil || !strings.Contains(catMgr, "Factory Task") || !strings.Contains(catMgr, "Human sign-off [signoff]") {
		t.Fatalf("catalog (mgr) = %q, err = %v", catMgr, err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", Call{Verb: "workflow", Args: []string{"catalog"}}); err != nil {
		t.Fatalf("catalog (worker) err = %v", err)
	}

	// Request proposal #1, then replace it with proposal #2 using --replaces and --repo.
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: "Factory Task", Summary: "Draft 1", Text: "Initial brief",
	}); err != nil {
		t.Fatalf("request #1: %v", err)
	}
	var prop1 string
	_ = r.d.Owner.QueryRow(ctx, `SELECT proposal_id::text FROM grimm_workflow_runs WHERE session_id = $1`, sid).Scan(&prop1)

	// Worker cannot withdraw proposal #1.
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", Call{
		Verb: "workflow", Args: []string{"withdraw", prop1}, Reason: "nope",
	}); err == nil || !strings.Contains(err.Error(), "only the Grimm manager") {
		t.Fatalf("worker withdraw err = %v, want 403", err)
	}

	// Manager replaces proposal #1 with proposal #2 and pins --repo.
	out, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: "Factory Task", Replaces: prop1, Repo: "/w/app", Summary: "Draft 2", Text: "Refined brief",
	})
	if err != nil || !strings.Contains(out, "replaced") {
		t.Fatalf("request --replaces = %q, err = %v", out, err)
	}
	var prop1Status string
	_ = r.d.Owner.QueryRow(ctx, `SELECT status FROM workflow_proposals WHERE id = $1`, prop1).Scan(&prop1Status)
	if prop1Status != "withdrawn" {
		t.Fatalf("prop1 status = %q, want withdrawn", prop1Status)
	}
	var prop2, prop2Input string
	_ = r.d.Owner.QueryRow(ctx, `SELECT p.id::text, p.input FROM workflow_proposals p JOIN grimm_workflow_runs g ON g.proposal_id = p.id WHERE g.session_id = $1 AND p.id <> $2`, sid, prop1).Scan(&prop2, &prop2Input)
	if !strings.HasPrefix(prop2Input, "Repo: /w/app\n\n") {
		t.Fatalf("prop2Input = %q, want Repo prefix", prop2Input)
	}

	// Request proposal #3 and withdraw it directly.
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: wfID, Summary: "Draft 3", Text: "To withdraw",
	}); err != nil {
		t.Fatalf("request #3: %v", err)
	}
	var prop3 string
	_ = r.d.Owner.QueryRow(ctx, `SELECT proposal_id::text FROM grimm_workflow_runs WHERE session_id = $1 AND proposal_id NOT IN ($2, $3)`, sid, prop1, prop2).Scan(&prop3)
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"withdraw", prop3}, Reason: "Not needed",
	}); err != nil {
		t.Fatalf("withdraw prop3: %v", err)
	}

	// Approve proposal #2 to start a run.
	if err := wf.Decide(ctx, r.acct, prop2, "approve"); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	var runID string
	_ = r.d.Owner.QueryRow(ctx, `SELECT run_id::text FROM grimm_workflow_run_status WHERE proposal_id = $1`, prop2).Scan(&runID)
	r.exec(`UPDATE workflow_runs SET status = 'running', step = 0, orchestrator_id = 'orch-1' WHERE id = $1`, runID)
	r.exec(`INSERT INTO transcripts (host, agent_id, epoch, synced_at) VALUES ('host-1', 'orch-1', 'e1', now())`)
	r.exec(`INSERT INTO transcript_entries (host, agent_id, seq_start, seq_end, item) VALUES ('host-1', 'orch-1', 1, 1, '{"type":"assistant_message","text":"Working on build step"}')`)

	// Both manager and worker can read events and peek orchestrator transcript.
	if evOut, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", Call{Verb: "workflow", Args: []string{"events", runID}, Lines: 10}); err != nil || evOut == "" {
		t.Fatalf("worker events = %q, err = %v", evOut, err)
	}
	if pkOut, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", Call{Verb: "workflow", Args: []string{"peek", runID}, Agent: "orchestrator", Lines: 10}); err != nil || !strings.Contains(pkOut, "Working on build step") {
		t.Fatalf("worker peek = %q, err = %v", pkOut, err)
	}

	// MoveRun MUST refuse Grimm agents (both manager and worker) for next/back/skip/done/fail/steer.
	for _, mv := range []string{"next", "back", "skip", "done", "fail", "steer"} {
		for _, ag := range []string{"mgr", "wrk-1"} {
			_, err := wf.MoveRun(workflow.WithCallerAgentID(ctx, ag), "host-1", runID, workflow.Move{Move: mv, Summary: "hijack", Why: "hijack", Step: "build", Into: runID, Note: "x"})
			if err == nil || !strings.Contains(err.Error(), "Grimm agents cannot move workflow runs") {
				t.Fatalf("MoveRun(%s as %s) = %v, want forbidden refusal", mv, ag, err)
			}
		}
	}

	// Worker gets 403 on input, reply, cancel.
	for _, sub := range []Call{
		{Verb: "workflow", Args: []string{"input", runID}, Text: "hi"},
		{Verb: "workflow", Args: []string{"reply", runID}, Text: "hi"},
		{Verb: "workflow", Args: []string{"cancel", runID}, Reason: "stop"},
	} {
		if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "wrk-1", sub); err == nil || !strings.Contains(err.Error(), "only the Grimm manager") {
			t.Fatalf("worker %v err = %v, want 403", sub.Args, err)
		}
	}

	// Manager sends MaxGrimmInputsPerRun inputs -> all succeed; next hits 429 rate limit.
	for i := range MaxGrimmInputsPerRun {
		r.later(time.Second)
		if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
			Verb: "workflow", Args: []string{"input", runID}, Text: "Guidance " + string(rune('A'+i)),
		}); err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"input", runID}, Text: "Too many",
	}); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("expected 429 rate limit, got %v", err)
	}
	// Verify grimm_session_id was recorded on workflow_run_events and orchestrator message attributes Grimm.
	var grimmEvtCount int
	_ = r.d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_run_events WHERE run_id = $1 AND grimm_session_id = $2 AND kind = 'input'`, runID, sid).Scan(&grimmEvtCount)
	if grimmEvtCount != MaxGrimmInputsPerRun {
		t.Fatalf("grimm input events = %d, want %d", grimmEvtCount, MaxGrimmInputsPerRun)
	}
	var cmdText string
	_ = r.d.Owner.QueryRow(ctx, `SELECT args->>'text' FROM commands WHERE host = 'host-1' AND kind = 'send_message' AND args->>'agentId' = 'orch-1' ORDER BY created_at DESC LIMIT 1`).Scan(&cmdText)
	if !strings.Contains(cmdText, "Additional input from Grimm") || !strings.Contains(cmdText, "not the human directly") {
		t.Fatalf("orchestrator input text missing Grimm attribution: %q", cmdText)
	}

	// Human posts a new message in the Grimm thread -> resets per-run input rate limit!
	r.later(time.Second)
	if _, err := r.s.Post(ctx, r.acct, sid, "Keep going, check edge cases too"); err != nil {
		t.Fatalf("human Post: %v", err)
	}
	r.later(time.Second)
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"input", runID}, Text: "Relayed after human message",
	}); err != nil {
		t.Fatalf("input after human reset failed: %v", err)
	}

	// Clarification ask: manager can reply.
	if _, err := wf.MoveRun(ctx, "host-1", runID, workflow.Move{Move: "ask", Note: "Which config file should I read?"}); err != nil {
		t.Fatalf("ask clarification: %v", err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"reply", runID}, Text: "Use config/prod.yaml",
	}); err != nil {
		t.Fatalf("reply to clarification: %v", err)
	}

	// Secret ask: manager MUST be hard-refused with 403.
	if _, err := wf.MoveRun(ctx, "host-1", runID, workflow.Move{
		Move:    "ask",
		Note:    "Need API token",
		Secrets: []workflow.SecretRequest{{Name: "API_TOKEN", Purpose: "deploy"}},
	}); err != nil {
		t.Fatalf("ask secret: %v", err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"reply", runID}, Text: "fake-secret",
	}); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("reply to secret ask = %v, want 403 refusal", err)
	}

	// Sign-off step: both reply AND input MUST be hard-refused with 403.
	r.exec(`UPDATE workflow_runs SET status = 'waiting', step = 1, note = 'Approve release?' WHERE id = $1`, runID)
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"reply", runID}, Text: "Approved!",
	}); err == nil || !strings.Contains(err.Error(), "sign-off") {
		t.Fatalf("reply on sign-off = %v, want 403 refusal", err)
	}
	r.exec(`UPDATE workflow_runs SET status = 'running', step = 1 WHERE id = $1`, runID)
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"input", runID}, Text: "Go ahead and merge",
	}); err == nil || !strings.Contains(err.Error(), "sign-off") {
		t.Fatalf("input during requireHumanSignoff = %v, want 403 refusal", err)
	}

	// Cancel: manager cancels with reason; workflow_run_events records grimm_session_id.
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"cancel", runID}, Reason: "Human instructed cancellation",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var runStatus, cancelText string
	_ = r.d.Owner.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&runStatus)
	_ = r.d.Owner.QueryRow(ctx, `SELECT text FROM workflow_run_events WHERE run_id = $1 AND kind = 'cancel' AND grimm_session_id = $2`, runID, sid).Scan(&cancelText)
	if runStatus != "canceled" || !strings.Contains(cancelText, "Human instructed cancellation") {
		t.Fatalf("status=%q cancelText=%q", runStatus, cancelText)
	}
}

func TestRicherWorkflowWakeups(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	wf := workflow.NewEngine(r.d.Owner, r.s.now)
	r.s.Workflows = wf

	def := workflow.Starting
	def.Host = "host-1"
	def.Orchestrator.Provider = "antigravity"
	def.Orchestrator.Model = "gemini-4-neon-medium"
	def.Orchestrator.Thinking = ""
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{
		{ID: "build", Kind: workflow.KindStep, Title: "Build", Instructions: "Build"},
		{ID: "verify", Kind: workflow.KindGate, Title: "Verify", Instructions: "Verify"},
		{ID: "signoff", Kind: workflow.KindSignoff, Title: "Sign-off", Instructions: "Sign off", RequireHumanSignoff: true},
	}
	if _, err := wf.CreateWorkflow(ctx, r.acct, "Release", "Release pipeline", def); err != nil {
		t.Fatal(err)
	}
	sid := r.ready()
	r.setAgent("mgr", "idle")

	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb: "workflow", Args: []string{"request"}, Workflow: "Release", Text: "Ship v3",
	}); err != nil {
		t.Fatal(err)
	}
	var propID string
	_ = r.d.Owner.QueryRow(ctx, `SELECT proposal_id::text FROM grimm_workflow_runs WHERE session_id = $1`, sid).Scan(&propID)
	if err := wf.Decide(ctx, r.acct, propID, "approve"); err != nil {
		t.Fatal(err)
	}
	var runID string
	_ = r.d.Owner.QueryRow(ctx, `SELECT run_id::text FROM grimm_workflow_run_status WHERE proposal_id = $1`, propID).Scan(&runID)

	// 1. Queued with reason
	r.exec(`UPDATE workflow_runs SET status = 'queued', queue_reason = 'Waiting for memory headroom on host-1' WHERE id = $1`, runID)
	r.reconcile(sid)
	var detail string
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "Waiting for memory headroom on host-1") {
		t.Fatalf("queued wakeup missing queue_reason: %q", detail)
	}
	if strings.Contains(detail, "Zach") {
		t.Fatalf("wakeup should not hardcode personal name: %q", detail)
	}

	// 2. Dequeued -> running
	r.exec(`UPDATE workflow_runs SET status = 'running', step = 0, queue_reason = '' WHERE id = $1`, runID)
	r.reconcile(sid)
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "dequeued") {
		t.Fatalf("expected dequeued wakeup, got: %q", detail)
	}

	// 3. Gate send-back (back event)
	r.exec(`UPDATE workflow_runs SET status = 'running', step = 0 WHERE id = $1`, runID)
	r.exec(`INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, to_step, text) VALUES ($1, $2, now(), 'back', 1, 0, 'Unit test TestFoo failed with race')`, runID, r.acct)
	r.reconcile(sid)
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "sent back") || !strings.Contains(detail, "Unit test TestFoo failed with race") {
		t.Fatalf("back wakeup = %q", detail)
	}

	// 4. Waiting clarification with > 800-char question (must NOT be truncated at 600 chars!)
	longQ := "Question prefix: " + strings.Repeat("detail ", 120) + "TAIL_MARKER_999"
	r.exec(`UPDATE workflow_runs SET status = 'waiting', step = 0, note = $2 WHERE id = $1`, runID, longQ)
	r.reconcile(sid)
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "TAIL_MARKER_999") || !strings.Contains(detail, "ask_kind=clarification") {
		t.Fatalf("clarification wakeup truncated or missing ask_kind: %q", detail)
	}
	if strings.Contains(detail, "Zach") {
		t.Fatalf("wakeup should not hardcode personal name: %q", detail)
	}

	// 5. Waiting signoff
	r.exec(`UPDATE workflow_runs SET status = 'waiting', step = 2, note = 'Ready to land?' WHERE id = $1`, runID)
	r.reconcile(sid)
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "ask_kind=signoff") {
		t.Fatalf("signoff wakeup = %q", detail)
	}

	// 6. Succeeded with summary + deliverables
	r.exec(`INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text) VALUES ($1, $2, now(), 'claim', 0, 'pr/42')`, runID, r.acct)
	r.exec(`INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text) VALUES ($1, $2, now(), 'done', 2, 'Shipped v3 cleanly')`, runID, r.acct)
	r.exec(`UPDATE workflow_runs SET status = 'succeeded' WHERE id = $1`, runID)
	r.reconcile(sid)
	_ = r.d.Owner.QueryRow(ctx, `SELECT detail FROM grimm_wakeups WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sid).Scan(&detail)
	if !strings.Contains(detail, "Shipped v3 cleanly") || !strings.Contains(detail, "pr/42") {
		t.Fatalf("succeeded wakeup missing summary/deliverables: %q", detail)
	}
}

func TestManagerStartRetryBackoffAndDeduplicatedNotices(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()

	sid, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		t.Fatal(err)
	}
	var wsCmd string
	_ = r.d.Owner.QueryRow(ctx, `SELECT workspace_command_id::text FROM grimm_sessions WHERE id = $1`, sid).Scan(&wsCmd)
	r.exec(`UPDATE commands SET status = 'succeeded', result = '{"workspaceId":"ws-1"}' WHERE id = $1`, wsCmd)
	r.reconcile(sid)

	// Remove all model groups so startManager fails deterministically.
	r.exec(`DELETE FROM model_groups`)

	// Human posts a message: 1st failure, 1 failed manager epoch, 1 notice.
	if _, err := r.s.Post(ctx, r.acct, sid, "Hello Grimm"); err != nil {
		t.Fatal(err)
	}
	counts := func() (epochs, notices int) {
		_ = r.d.Owner.QueryRow(ctx, `SELECT count(*) FROM grimm_managers WHERE session_id = $1 AND status = 'failed'`, sid).Scan(&epochs)
		_ = r.d.Owner.QueryRow(ctx, `SELECT count(*) FROM grimm_messages WHERE session_id = $1 AND kind = 'notice'`, sid).Scan(&notices)
		return
	}
	ep, nt := counts()
	if ep != 1 || nt != 1 {
		t.Fatalf("after 1st failure: epochs=%d notices=%d, want 1, 1", ep, nt)
	}

	// Supervisor ticks 10 times within 4 seconds: exponential backoff prevents retry storm!
	for range 10 {
		r.later(400 * time.Millisecond)
		r.reconcile(sid)
	}
	ep, nt = counts()
	if ep != 1 || nt != 1 {
		t.Fatalf("during 5s backoff: epochs=%d notices=%d, want 1, 1", ep, nt)
	}

	// Advance past 5s initial backoff: retries once (2nd failure), notice stays deduplicated at 1!
	r.later(2 * time.Second)
	r.reconcile(sid)
	ep, nt = counts()
	if ep != 2 || nt != 1 {
		t.Fatalf("after 5s backoff: epochs=%d notices=%d, want 2, 1", ep, nt)
	}

	// Advance 9s (< 10s second backoff): must NOT retry.
	r.later(9 * time.Second)
	r.reconcile(sid)
	ep, nt = counts()
	if ep != 2 || nt != 1 {
		t.Fatalf("during 10s backoff: epochs=%d notices=%d, want 2, 1", ep, nt)
	}

	// Advance 2s (>= 10s): retries (3rd failure), notice STILL deduplicated at 1!
	r.later(2 * time.Second)
	r.reconcile(sid)
	ep, nt = counts()
	if ep != 3 || nt != 1 {
		t.Fatalf("after 10s backoff: epochs=%d notices=%d, want 3, 1", ep, nt)
	}

	// Human posts a new message: retries immediately regardless of backoff!
	r.later(time.Second)
	if _, err := r.s.Post(ctx, r.acct, sid, "Try again now"); err != nil {
		t.Fatal(err)
	}
	ep, _ = counts()
	if ep != 4 {
		t.Fatalf("after human retry message: epochs=%d, want 4", ep)
	}
}
