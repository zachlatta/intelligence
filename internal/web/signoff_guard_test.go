package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRequireHumanSignoffEnforcesTypedBrowserApproval(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "test")
	reg := fleet.New(d.Owner)
	wfEng := workflow.NewEngine(d.Owner, nil)
	ctx := context.Background()
	hostKey := keyOf(t, h, "host-1")
	if err := reg.Record(ctx, wfReport(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// requireHumanSignoff is rejected on non-signoff steps.
	invalidDef := apiWorkflowDef("host-1")
	invalidDef.Steps[0].RequireHumanSignoff = true
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Invalid Guard", invalidDef), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Fatalf("requireHumanSignoff on step kind=%s: got %d, want 400 (%s)", invalidDef.Steps[0].Kind, rec.Code, rec.Body)
	}

	// Build a workflow with a mandatory typed human sign-off step before landing.
	def := workflow.Definition{
		Host:       "host-1",
		Trigger:    workflow.TriggerManual,
		APITrigger: true,
		Project:    workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"},
		Orchestrator: workflow.Orchestrator{
			Provider:     "claude",
			Model:        "opus",
			Instructions: "Orchestrate the guarded fix.",
		},
		Steps: []workflow.Step{
			{ID: "build", Kind: workflow.KindStep, Title: "Build", Instructions: "Build {{input}}."},
			{ID: "signoff", Kind: workflow.KindSignoff, Title: "Human sign-off", Instructions: "Ask for approval.", RequireHumanSignoff: true},
			{ID: "land", Kind: workflow.KindStep, Title: "Land", Instructions: "Land approved change."},
		},
	}
	wfID := createWorkflow(t, h, def)
	_, apiKey := issueAPIKey(t, h, wfID, "outliner-fix")

	// Metadata exposes requireHumanSignoff on the sign-off step.
	metaRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/workflow", apiKey, "")
	if metaRec.Code != http.StatusOK || !strings.Contains(metaRec.Body.String(), `"requireHumanSignoff":true`) {
		t.Fatalf("GET workflow metadata = %d %s", metaRec.Code, metaRec.Body)
	}

	// Start a run via API key and simulate orchestrator launch.
	startRec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", apiKey, `{"input":"Fix synthetic contract"}`)
	if startRec.Code != http.StatusCreated {
		t.Fatalf("POST start run = %d %s", startRec.Code, startRec.Body)
	}
	var run apiRunSummary
	if err := json.Unmarshal(startRec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	var launchCmdID string
	if err := d.Owner.QueryRow(ctx, `SELECT pending_command::text FROM workflow_runs WHERE id = $1`, run.ID).Scan(&launchCmdID); err != nil {
		t.Fatal(err)
	}
	resJSON, _ := json.Marshal(hostclient.CommandResult{WorkspaceID: "ws-guard", AgentID: "orch-guard"})
	if err := reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: launchCmdID, Status: hostclient.CommandSucceeded, Result: resJSON}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := wfEng.CommandUpdated(ctx, launchCmdID); err != nil {
		t.Fatal(err)
	}

	// approve: true is rejected on non-signoff steps (400).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Quick question during build?"}); code != http.StatusOK {
		t.Fatalf("ask on build = %d %s", code, msg)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"Yes","approve":true}`, sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Fatalf("approve:true on non-signoff step = %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"Yes"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("normal reply on build = %d %s", rec.Code, rec.Body)
	}

	// Steering input is still allowed during Step 0 (build).
	if rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/input", apiKey, `{"text":"Note during build"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST input on build step = %d %s", rec.Code, rec.Body)
	}

	// Advance to Step 1 (signoff).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Built"}); code != http.StatusOK {
		t.Fatalf("move to signoff = %d %s", code, msg)
	}

	// Negative tests before any ask:
	// 1. Skip is refused (409).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "skip", "why": "criteria hash matched"}); code != http.StatusConflict {
		t.Fatalf("skip on requireHumanSignoff = %d %s, want 409", code, msg)
	}
	// 2. Next without ask/approval is refused (409).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "proceed"}); code != http.StatusConflict {
		t.Fatalf("next without approval = %d %s, want 409", code, msg)
	}
	// 3. API-key steering input on sign-off step is refused (403).
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/input", apiKey, `{"text":"approve"}`), http.StatusForbidden)

	// Orchestrator asks for sign-off.
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Before/after diff ready. Approve?"}); code != http.StatusOK {
		t.Fatalf("ask on signoff = %d %s", code, msg)
	}

	// 4. API-key reply (including attempting approve:true or text:"approve") is refused (400/403) and leaves run waiting.
	var cmdCountBefore int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM commands WHERE host='host-1' AND kind='send_message'`).Scan(&cmdCountBefore)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/reply", apiKey, `{"text":"approve"}`), http.StatusForbidden)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/reply", apiKey, `{"text":"approve","approve":true}`), http.StatusBadRequest)
	// 5. Email-path reply (Reply / ReplyTx with no browser session, as email/service.go uses) is refused with Forbidden and leaves run waiting.
	var acctID int64
	if err := d.Owner.QueryRow(ctx, `SELECT account_id FROM workflow_runs WHERE id = $1`, run.ID).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	emailErr := wfEng.Reply(ctx, acctID, run.ID, "approve")
	var emailRef *workflow.Refusal
	if !errors.As(emailErr, &emailRef) || !emailRef.Forbidden {
		t.Fatalf("email-path Reply on requireHumanSignoff = %v, want Forbidden refusal", emailErr)
	}
	var cmdCountAfter int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM commands WHERE host='host-1' AND kind='send_message'`).Scan(&cmdCountAfter)
	if cmdCountAfter != cmdCountBefore {
		t.Fatalf("refused API/email replies enqueued send_message: before=%d after=%d", cmdCountBefore, cmdCountAfter)
	}
	var status string
	_ = d.Owner.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, run.ID).Scan(&status)
	if status != workflow.Waiting {
		t.Fatalf("status after refused API/email replies = %q, want waiting", status)
	}

	// 6. Browser-session REJECTION reply ("REJECTED. Do not land this.") wakes orchestrator (204)
	//    but MUST NOT allow orchestrator next (409)!
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"REJECTED. Do not land this. Cancel."}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("browser rejection reply = %d %s", rec.Code, rec.Body)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "ignoring rejection"}); code != http.StatusConflict {
		t.Fatalf("next after browser REJECTION reply = %d %s, want 409", code, msg)
	}

	// 7. Browser-session plain text reply saying "approve" WITHOUT approve:true ALSO MUST NOT unlock next (409)!
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Clarifying diff. Approve?"}); code != http.StatusOK {
		t.Fatalf("second ask on signoff = %d %s", code, msg)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"approve"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("browser plain-text 'approve' reply = %d %s", rec.Code, rec.Body)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "plain text approve"}); code != http.StatusConflict {
		t.Fatalf("next after plain-text 'approve' without approve:true = %d %s, want 409", code, msg)
	}

	// 8. Browser explicit approval (approve:true) followed by a re-ask within the same visit invalidates the earlier approval.
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Ready for approval."}); code != http.StatusOK {
		t.Fatalf("third ask on signoff = %d %s", code, msg)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"Looks good","approve":true}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("browser explicit approval = %d %s", rec.Code, rec.Body)
	}
	// Suppose orchestrator discovers a risk before advancing and asks again:
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Wait: found extra downtime. Still approve?"}); code != http.StatusOK {
		t.Fatalf("re-ask after approval = %d %s", code, msg)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"No, revise scope first"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("reply to re-ask = %d %s", rec.Code, rec.Body)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "replaying approval from before re-ask"}); code != http.StatusConflict {
		t.Fatalf("next after re-ask without fresh approve:true = %d %s, want 409", code, msg)
	}

	// 9. Scope revision loop (back to build -> return to signoff): old visit approval cannot be replayed!
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "back", "step": "build", "why": "scope revised"}); code != http.StatusOK {
		t.Fatalf("back to build = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Rebuilt revised scope"}); code != http.StatusOK {
		t.Fatalf("second entry to signoff = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "replaying old visit approval"}); code != http.StatusConflict {
		t.Fatalf("next on second signoff visit without fresh ask/approval = %d %s, want 409", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Revised scope diff ready. Approve?"}); code != http.StatusOK {
		t.Fatalf("ask on second signoff visit = %d %s", code, msg)
	}
	// Run page HTML renders explicit Approve sign-off button for requireHumanSignoff step.
	pageRec := call(t, h, http.MethodGet, "/workflow-runs/"+run.ID, "", nil)
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), `id="approve-signoff"`) {
		t.Fatalf("GET run page missing approve-signoff button: code=%d", pageRec.Code)
	}
	// API reply still refused (403).
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/reply", apiKey, `{"text":"approve revised scope"}`), http.StatusForbidden)
	// Explicit browser approval with empty text defaults to "Approved" and unlocks next!
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"","approve":true}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("fresh browser explicit approval = %d %s", rec.Code, rec.Body)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Explicitly approved by Zach"}); code != http.StatusOK {
		t.Fatalf("next after fresh explicit approval = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "done", "summary": "Landed"}); code != http.StatusOK {
		t.Fatalf("done on land = %d %s", code, msg)
	}
}
