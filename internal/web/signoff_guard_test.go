package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRequireHumanSignoffEnforcesAuthenticatedHumanApproval(t *testing.T) {
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

	// Build a workflow with a mandatory human sign-off step before landing.
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

	// Steering input is still allowed during Step 0 (build).
	if rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/input", apiKey, `{"text":"Note during build"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST input on build step = %d %s", rec.Code, rec.Body)
	}

	// Advance to Step 1 (signoff).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Built"}); code != http.StatusOK {
		t.Fatalf("move to signoff = %d %s", code, msg)
	}

	// Negative tests on sign-off step before any ask/human reply:
	// 1. Skip is refused (409).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "skip", "why": "criteria hash matched"}); code != http.StatusConflict {
		t.Fatalf("skip on requireHumanSignoff = %d %s, want 409", code, msg)
	}
	// 2. Next without ask + human reply is refused (409).
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "proceed"}); code != http.StatusConflict {
		t.Fatalf("next without human reply = %d %s, want 409", code, msg)
	}
	// 3. API-key steering input on sign-off step is refused (403).
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/input", apiKey, `{"text":"approve"}`), http.StatusForbidden)

	// Orchestrator asks for sign-off.
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Before/after diff ready. Approve?"}); code != http.StatusOK {
		t.Fatalf("ask on signoff = %d %s", code, msg)
	}

	// 4. API-key reply with approval-looking text is refused (403) and leaves run waiting with no command enqueued.
	var cmdCountBefore int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM commands WHERE host='host-1' AND kind='send_message'`).Scan(&cmdCountBefore)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/reply", apiKey, `{"text":"approve"}`), http.StatusForbidden)
	var cmdCountAfter int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM commands WHERE host='host-1' AND kind='send_message'`).Scan(&cmdCountAfter)
	if cmdCountAfter != cmdCountBefore {
		t.Fatalf("API reply on human sign-off enqueued send_message: before=%d after=%d", cmdCountBefore, cmdCountAfter)
	}
	var status string
	_ = d.Owner.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, run.ID).Scan(&status)
	if status != workflow.Waiting {
		t.Fatalf("status after refused API reply = %q, want waiting", status)
	}

	// 5. Authenticated browser-session human reply succeeds and unlocks moving from signoff.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"Please adjust scope first"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("browser session reply = %d %s", rec.Code, rec.Body)
	}

	// Suppose scope changes: orchestrator loops back to build, then returns to signoff.
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "back", "step": "build", "why": "scope revised"}); code != http.StatusOK {
		t.Fatalf("back to build = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Rebuilt revised scope"}); code != http.StatusOK {
		t.Fatalf("second entry to signoff = %d %s", code, msg)
	}

	// 6. Earlier visit's human reply CANNOT authorize the revised scope! Must ask and receive a fresh human reply.
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "replaying old approval"}); code != http.StatusConflict {
		t.Fatalf("next on second signoff visit without fresh ask/reply = %d %s, want 409", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "ask", "note": "Revised scope diff ready. Approve?"}); code != http.StatusOK {
		t.Fatalf("second ask on signoff = %d %s", code, msg)
	}
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+run.ID+"/reply", apiKey, `{"text":"approve revised scope"}`), http.StatusForbidden)
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run.ID+"/reply", `{"text":"approve"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("second browser session reply = %d %s", rec.Code, rec.Body)
	}

	// Now advancing to Land succeeds!
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "next", "summary": "Approved by Zach"}); code != http.StatusOK {
		t.Fatalf("next after fresh human reply = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, run.ID, map[string]string{"move": "done", "summary": "Landed"}); code != http.StatusOK {
		t.Fatalf("done on land = %d %s", code, msg)
	}
}
