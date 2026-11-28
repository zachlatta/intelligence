package grimm

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestArchiveSessionStopsAndArchivesAgentsCleansWorktreeIfCleanAndKeepsHistory(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	wf := workflow.NewEngine(r.d.Owner, time.Now)
	r.s.Workflows = wf

	id := r.ready()
	r.started(id, "athena", "w1")
	r.started(id, "hermes", "w2")
	r.setAgent("w2", "idle")
	r.reconcile(id)

	// Archive a third worker beforehand; ending the session must not re-archive it.
	r.started(id, "apollo", "w3")
	if _, err := r.s.ArchiveWorkers(ctx, r.acct, id, "apollo"); err != nil {
		t.Fatal(err)
	}
	// Drain queued commands from setup.
	r.exec(`UPDATE commands SET status = 'sent' WHERE status = 'queued'`)

	// Add a pending workflow proposal linked to this session to verify ending leaves it pending.
	def := workflow.Starting
	def.Host = "host-1"
	def.Orchestrator.Provider = "antigravity"
	def.Orchestrator.Model = "gemini-4-neon-medium"
	def.Orchestrator.Thinking = ""
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{{ID: "s1", Kind: workflow.KindStep, Title: "Check", Instructions: "Check it"}}
	wfID, err := wf.CreateWorkflow(ctx, r.acct, "Deploy Check", "Check release", def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{
		Verb:     "workflow",
		Args:     []string{"request"},
		Workflow: wfID,
		Summary:  "Run deploy check",
		Text:     "Verify release readiness",
	}); err != nil {
		t.Fatal(err)
	}

	// Queue a pending wakeup to confirm ending drops it.
	r.exec(`INSERT INTO grimm_wakeups (session_id, account_id, worker, summary, detail) VALUES ($1, $2, 'athena', 's', 'd')`, id, r.acct)

	if err := r.s.ArchiveSession(ctx, r.acct, id); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Check queued commands: cancel + archive for mgr, w1, w2; archive_workspace with onlyIfClean:true for ws-1.
	cmds := r.commands()
	canceled := map[string]bool{}
	archived := map[string]bool{}
	var wsArchive *cmd
	for i := range cmds {
		c := cmds[i]
		switch c.Kind {
		case hostclient.CancelAgentKind:
			canceled[c.Args["agentId"].(string)] = true
		case hostclient.ArchiveAgentKind:
			archived[c.Args["agentId"].(string)] = true
		case hostclient.ArchiveWorkspaceKind:
			wsArchive = &cmds[i]
		}
	}
	for _, ag := range []string{"mgr", "w1", "w2"} {
		if !canceled[ag] {
			t.Errorf("missing cancel_agent for %s; commands = %+v", ag, cmds)
		}
		if !archived[ag] {
			t.Errorf("missing archive_agent for %s; commands = %+v", ag, cmds)
		}
	}
	if canceled["w3"] || archived["w3"] {
		t.Errorf("already-archived worker w3 was touched: canceled=%v archived=%v", canceled, archived)
	}
	if wsArchive == nil {
		t.Fatalf("missing archive_workspace command; commands = %+v", cmds)
	}
	if wsArchive.Args["workspaceId"] != "ws-1" || wsArchive.Args["onlyIfClean"] != true {
		t.Errorf("archive_workspace args = %+v, want workspaceId=ws-1 onlyIfClean=true", wsArchive.Args)
	}

	// Session, manager, workers, wakeups updated cleanly.
	if got := r.one(`SELECT status FROM grimm_sessions WHERE id = $1`, id); got != "archived" {
		t.Errorf("session status = %v, want archived", got)
	}
	if got := r.one(`SELECT archived_at IS NOT NULL FROM grimm_sessions WHERE id = $1`, id); got != true {
		t.Error("archived_at is null")
	}
	if got := r.one(`SELECT status FROM grimm_managers WHERE session_id = $1 ORDER BY epoch DESC LIMIT 1`, id); got != "ended" {
		t.Errorf("manager status = %v, want ended", got)
	}
	if got := r.one(`SELECT count(*) FROM grimm_workers WHERE session_id = $1 AND status <> 'archived'`, id); got != int64(0) {
		t.Errorf("non-archived workers = %v, want 0", got)
	}
	if got := r.one(`SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND delivered_at IS NULL`, id); got != int64(0) {
		t.Errorf("undelivered wakeups = %v, want 0", got)
	}

	// Pending workflow proposal stays pending (Zach decides proposals on their own page).
	if got := r.one(`SELECT p.status FROM grimm_workflow_runs g JOIN workflow_proposals p ON p.id = g.proposal_id WHERE g.session_id = $1`, id); got != "pending" {
		t.Errorf("pending proposal status = %v, want pending", got)
	}

	// Thread divider recorded and history stays readable.
	if got := r.one(`SELECT count(*) FROM grimm_thread WHERE session_id = $1 AND kind = 'divider' AND body = 'Session ended'`, id); got != int64(1) {
		t.Errorf("Session ended divider count = %v, want 1", got)
	}
	if got := r.one(`SELECT count(*) FROM grimm_workers WHERE session_id = $1`, id); got != int64(3) {
		t.Errorf("roster count = %v, want 3", got)
	}

	// Ending again returns 409 conflict; wrong account returns 404.
	if err := r.s.ArchiveSession(ctx, r.acct, id); err == nil || err.(*Error).Status != 409 {
		t.Errorf("second ArchiveSession = %v, want 409", err)
	}
	if err := r.s.ArchiveSession(ctx, r.acct+999, id); err == nil || err.(*Error).Status != 404 {
		t.Errorf("other account ArchiveSession = %v, want 404", err)
	}
}
