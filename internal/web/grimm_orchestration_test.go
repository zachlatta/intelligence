package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestGrimmAgentLargePayloadMoveRunGuardAndRunPageAttribution(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	d := h.(*testServer).d
	ctx := t.Context()

	// Create a workflow on host-1.
	def := workflow.Starting
	def.Host = "host-1"
	def.Orchestrator.Provider = "antigravity"
	def.Orchestrator.Model = "gemini-4-neon-medium"
	def.Orchestrator.Thinking = ""
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{
		{ID: "implement", Kind: workflow.KindStep, Title: "Implement", Instructions: "Do it"},
		{ID: "signoff", Kind: workflow.KindSignoff, Title: "Sign-off", Instructions: "Ask", RequireHumanSignoff: true},
	}
	wfBodyJSON, _ := json.Marshal(map[string]any{"name": "Factory Task", "description": "Ship changes", "definition": def})
	if rec := call(t, h, "POST", "/api/workflows", string(wfBodyJSON), sameOriginFetch); rec.Code != 201 {
		t.Fatalf("create workflow: %d %s", rec.Code, rec.Body)
	}

	// Create a Grimm session and start the manager.
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":5}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"ship feature"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("post message: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"mgr-large-1"}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var aid string
		_ = d.Owner.QueryRow(ctx, `SELECT agent_id FROM grimm_managers WHERE session_id = $1`, created.ID).Scan(&aid)
		if aid == "mgr-large-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for manager mgr-large-1")
		}
		time.Sleep(25 * time.Millisecond)
	}

	hostKey := keyOf(t, h, "host-1")
	agentHeaders := map[string]string{
		"Authorization":   "Bearer " + hostKey,
		grimm.AgentHeader: "mgr-large-1",
		"Content-Type":    "application/json",
	}

	// Regression: /api/grimm/agent MUST accept > 64 KiB workflow request payloads without 400/truncation.
	largeInput := "Repo: /w/app\n\n" + strings.Repeat("B", 120*1024) + "\nEND_MARKER"
	reqBody, _ := json.Marshal(grimm.Call{
		Verb:     "workflow",
		Args:     []string{"request"},
		Workflow: "Factory Task",
		Summary:  "Large researched brief",
		Text:     largeInput,
	})
	if r := call(t, anon(h), "POST", grimm.AgentPath, string(reqBody), agentHeaders); r.Code != 200 {
		t.Fatalf("large agent workflow request: %d %s", r.Code, r.Body)
	}

	var propID, storedInput string
	if err := d.Owner.QueryRow(ctx, `SELECT g.proposal_id::text, p.input FROM grimm_workflow_runs g JOIN workflow_proposals p ON p.id = g.proposal_id WHERE g.session_id = $1`, created.ID).Scan(&propID, &storedInput); err != nil {
		t.Fatalf("find proposal: %v", err)
	}
	if len(storedInput) != len(largeInput) || !strings.HasSuffix(storedInput, "END_MARKER") {
		t.Fatalf("storedInput truncated: len=%d want=%d", len(storedInput), len(largeInput))
	}

	// Approve proposal to start run.
	if appRec := call(t, h, "POST", "/api/workflow-proposals/"+propID+"/approve", `{}`, sameOriginFetch); appRec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", appRec.Code, appRec.Body)
	}
	var runID string
	_ = d.Owner.QueryRow(ctx, `SELECT started_run_id::text FROM workflow_proposals WHERE id = $1`, propID).Scan(&runID)
	_, _ = d.Owner.Exec(ctx, `UPDATE workflow_runs SET status = 'running', step = 0, orchestrator_id = 'orch-web-1' WHERE id = $1`, runID)

	// POST /api/workflow-runs/{id}/moves MUST refuse Grimm agent via X-Paseo-Agent-Id with 403.
	if mv := call(t, anon(h), "POST", "/api/workflow-runs/"+runID+"/moves", `{"move":"next","summary":"hijack"}`, agentHeaders); mv.Code != http.StatusForbidden {
		t.Fatalf("MoveRun from Grimm agent: status = %d %s, want 403", mv.Code, mv.Body)
	}

	// Manager sends mid-run input via /api/grimm/agent.
	inBody, _ := json.Marshal(grimm.Call{Verb: "workflow", Args: []string{"input", runID}, Text: "Prefer standard library"})
	if ir := call(t, anon(h), "POST", grimm.AgentPath, string(inBody), agentHeaders); ir.Code != 200 {
		t.Fatalf("agent workflow input: %d %s", ir.Code, ir.Body)
	}

	// Workflow run page MUST surface Grimm-originated input clearly.
	runPage := get(t, h, "/workflow-runs/"+runID, nil)
	if runPage.Code != 200 {
		t.Fatalf("run page: %d", runPage.Code)
	}
	if !strings.Contains(runPage.Body.String(), "Additional input from Grimm") || !strings.Contains(runPage.Body.String(), "/grimm/"+created.ID) || !strings.Contains(runPage.Body.String(), "Prefer standard library") {
		t.Fatalf("run page missing Grimm input attribution")
	}

	// Verify grimm_runs.js includes inline reply UI and browser-only sign-off distinction.
	jsBody := get(t, h, "/grimm_runs.js", nil).Body.String()
	for _, want := range []string{"gw-reply-area", "gw-guidance", "browser sign-off only", "/api/workflow-runs/"} {
		if !strings.Contains(jsBody, want) {
			t.Errorf("grimm_runs.js missing %q", want)
		}
	}
}
