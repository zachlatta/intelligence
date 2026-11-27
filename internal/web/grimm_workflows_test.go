package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestGrimmWorkflowRequestApproveRejectAndStream(t *testing.T) {
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
		{ID: "implement", Kind: workflow.KindStep, Title: "Implement", Instructions: "Do the work"},
		{ID: "gate", Kind: workflow.KindGate, Title: "Tests pass", Instructions: "Run tests"},
	}
	wfBodyJSON, _ := json.Marshal(map[string]any{"name": "Release Flow", "description": "Release pipeline", "definition": def})
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
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"please run Release Flow"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("post message: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"mgr-wf-1"}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var aid string
		_ = d.Owner.QueryRow(ctx, `SELECT agent_id FROM grimm_managers WHERE session_id = $1`, created.ID).Scan(&aid)
		if aid == "mgr-wf-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for manager mgr-wf-1 to be reconciled")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Manager requests a workflow run via POST /api/grimm/agent (host key).
	hostKey := keyOf(t, h, "host-1")
	reqBody, _ := json.Marshal(grimm.Call{
		Verb:     "workflow",
		Args:     []string{"request"},
		Workflow: "Release Flow",
		Summary:  "Run Release Flow for v1",
		Text:     "Initial task input",
	})
	agentRec := call(t, anon(h), "POST", grimm.AgentPath, string(reqBody), map[string]string{
		"Authorization":   "Bearer " + hostKey,
		grimm.AgentHeader: "mgr-wf-1",
		"Content-Type":    "application/json",
	})
	if agentRec.Code != 200 {
		t.Fatalf("agent workflow request: %d %s", agentRec.Code, agentRec.Body)
	}

	var propID string
	if err := d.Owner.QueryRow(ctx, `SELECT proposal_id::text FROM grimm_workflow_runs WHERE session_id = $1`, created.ID).Scan(&propID); err != nil {
		t.Fatalf("find proposal: %v", err)
	}

	// Host key MUST be refused from approving, rejecting, or editing input.
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/workflow-proposals/" + propID + "/approve", `{}`},
		{"/api/workflow-proposals/" + propID + "/reject", `{}`},
		{"/api/grimm/sessions/" + created.ID + "/workflow-requests/" + propID + "/input", `{"input":"hacked"}`},
	} {
		r := call(t, anon(h), "POST", tc.path, tc.body, map[string]string{
			"Authorization":   "Bearer " + hostKey,
			grimm.AgentHeader: "mgr-wf-1",
			"Content-Type":    "application/json",
		})
		if r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Fatalf("host key on %s: status = %d, want 401/403", tc.path, r.Code)
		}
	}

	// Zach edits the input before approving (browser session).
	editRec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/workflow-requests/"+propID+"/input",
		`{"input":"Updated task input by Zach"}`, sameOriginFetch)
	if editRec.Code != http.StatusNoContent {
		t.Fatalf("edit input: %d %s", editRec.Code, editRec.Body)
	}

	// Open the SSE stream and verify we receive workflow_runs with the updated input.
	srv := serve(t, h)
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sreq, _ := http.NewRequestWithContext(sctx, http.MethodGet, srv.URL+"/api/grimm/sessions/"+created.ID+"/stream", nil)
	sreq.AddCookie(h.(*testServer).cookie)
	sresp, err := srv.Client().Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()

	wfEvents := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(sresp.Body)
		sc.Buffer(make([]byte, 64<<10), 2<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") && ev == "workflow_runs" {
				wfEvents <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(8 * time.Second)
		for {
			select {
			case data := <-wfEvents:
				if strings.Contains(data, want) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for workflow_runs SSE containing %q", want)
			}
		}
	}

	waitFor("Updated task input by Zach")

	// Zach approves via the existing browser-only proposal route.
	appRec := call(t, h, "POST", "/api/workflow-proposals/"+propID+"/approve", `{}`, sameOriginFetch)
	if appRec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", appRec.Code, appRec.Body)
	}

	// SSE stream updates live with applied status and started run!
	waitFor(`"proposal_status":"applied"`)

	// Verify the run actually started with Zach's edited input.
	var runID, runInput string
	if err := d.Owner.QueryRow(ctx, `SELECT r.id::text, r.input FROM workflow_runs r JOIN workflow_proposals p ON p.started_run_id = r.id WHERE p.id = $1`, propID).Scan(&runID, &runInput); err != nil {
		t.Fatalf("started run: %v", err)
	}
	if runInput != "Updated task input by Zach" {
		t.Fatalf("run input = %q", runInput)
	}

	// Simulate the workflow run advancing to waiting and notifying wfrun:<id>: SSE updates live!
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_runs SET status = 'waiting', step = 1, note = 'All tests passed; ship?' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `SELECT pg_notify('fleet', 'wfrun:' || $1)`, runID); err != nil {
		t.Fatal(err)
	}
	waitFor(`"run_status":"waiting"`)

	// Request a second workflow and verify Zach can reject it via the existing browser route.
	reqBody2, _ := json.Marshal(grimm.Call{
		Verb:     "workflow",
		Args:     []string{"request"},
		Workflow: "Release Flow",
		Summary:  "Second request to reject",
		Text:     "Do not run this",
	})
	if r2 := call(t, anon(h), "POST", grimm.AgentPath, string(reqBody2), map[string]string{
		"Authorization":   "Bearer " + hostKey,
		grimm.AgentHeader: "mgr-wf-1",
		"Content-Type":    "application/json",
	}); r2.Code != 200 {
		t.Fatalf("second agent request: %d %s", r2.Code, r2.Body)
	}
	var propID2 string
	if err := d.Owner.QueryRow(ctx, `SELECT proposal_id::text FROM grimm_workflow_runs WHERE session_id = $1 AND proposal_id <> $2`, created.ID, propID).Scan(&propID2); err != nil {
		t.Fatalf("find second proposal: %v", err)
	}
	if rej := call(t, h, "POST", "/api/workflow-proposals/"+propID2+"/reject", `{}`, sameOriginFetch); rej.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rej.Code, rej.Body)
	}
	waitFor(`"proposal_status":"rejected"`)

	// Verify the Grimm page includes the new grimm_runs assets.
	page := get(t, h, created.URL, nil).Body.String()
	if !strings.Contains(page, "/grimm_runs.js") || !strings.Contains(page, "/grimm_runs.css") {
		t.Fatalf("grimm page missing grimm_runs assets")
	}
	_ = hostclient.CreateWorkspaceKind
}
