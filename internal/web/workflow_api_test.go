package web

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func apiWorkflowDef(host string) workflow.Definition {
	return workflow.Definition{
		Host:    host,
		Trigger: workflow.TriggerAPI,
		Project: workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"},
		Orchestrator: workflow.Orchestrator{
			Provider:     "claude",
			Model:        "opus",
			Instructions: "Orchestrate the contract check.",
		},
		Steps: []workflow.Step{
			{ID: "check", Kind: workflow.KindStep, Title: "Check contract", Instructions: "Review {{input}}."},
			{ID: "approve", Kind: workflow.KindSignoff, Title: "Sign-off", Instructions: "Ask if needed."},
		},
	}
}

func issueAPIKey(t *testing.T, h http.Handler, wfID, name string) (id, secret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	rec := call(t, h, http.MethodPost, "/api/workflows/"+wfID+"/api-keys", string(body), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/workflows/%s/api-keys = %d %s", wfID, rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || !strings.HasPrefix(out.Key, "iwfk_") {
		t.Fatalf("unexpected key response: %s", rec.Body)
	}
	return out.ID, out.Key
}

func callAPI(t *testing.T, h http.Handler, method, path, key, body string, extra ...map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	hdrs := map[string]string{}
	if key != "" {
		hdrs["Authorization"] = "Bearer " + key
	}
	for _, m := range extra {
		for k, v := range m {
			hdrs[k] = v
		}
	}
	return call(t, anon(h), method, path, body, hdrs)
}

func TestWorkflowAPIKeyScopingAuthAndControl(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "test")
	reg := fleet.New(d.Owner)
	wfEng := workflow.NewEngine(d.Owner, nil)
	ctx := context.Background()
	hostKey := keyOf(t, h, "host-1")
	if err := reg.Record(ctx, wfReport(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// Create two API-triggered workflows: A ("Agreement review") and B ("Other workflow").
	wfA := createWorkflow(t, h, apiWorkflowDef("host-1"))
	wfB := createWorkflow(t, h, apiWorkflowDef("host-1"))
	keyAID, keyA := issueAPIKey(t, h, wfA, "ci-prod")
	_, keyB := issueAPIKey(t, h, wfB, "other-repo")

	// 1. Auth boundary: API key cannot reach any non-workflow-API route (including SQL, workflows, MCP, key management).
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/sql", `{"query":"SELECT 1"}`},
		{http.MethodGet, "/api/hosts", ""},
		{http.MethodPost, "/api/workflows", wfBody(t, "x", apiWorkflowDef("host-1"))},
		{http.MethodPost, "/api/workflow-runs", `{"workflowId":"` + wfA + `","input":"hi"}`},
		{http.MethodPost, "/api/workflows/" + wfA + "/api-keys", `{"name":"nested"}`},
		{http.MethodDelete, "/api/workflows/" + wfA + "/api-keys/" + keyAID, ""},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`},
	} {
		rec := callAPI(t, h, tc.method, tc.path, keyA, tc.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with workflow API key = %d, want 401 (body: %s)", tc.method, tc.path, rec.Code, rec.Body)
		}
	}

	// 2. Missing, invalid, host key, and browser cookie are rejected on /api/workflow-api/*.
	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"invalid", "iwfk_" + strings.Repeat("z", 43), http.StatusUnauthorized},
		{"host key", hostKey, http.StatusUnauthorized},
	} {
		rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/workflow", tc.key, "")
		if rec.Code != tc.want {
			t.Errorf("auth %s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	// Browser session cookie without Bearer key is rejected on /api/workflow-api/*.
	if rec := call(t, h, http.MethodGet, "/api/workflow-api/workflow", "", sameOriginFetch); rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser cookie on /api/workflow-api/workflow = %d, want 401", rec.Code)
	}

	// 3. GET /api/workflow-api/workflow returns scoped workflow metadata.
	rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/workflow", keyA, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wfA) {
		t.Fatalf("GET /api/workflow-api/workflow = %d %s", rec.Code, rec.Body)
	}

	// 4. Trigger run with inline attachment + idempotency.
	attData := base64.StdEncoding.EncodeToString([]byte("Contract text body"))
	triggerBody := `{"input":"Check Acme MSA","attachments":[{"name":"msa.txt","data":"` + attData + `"}]}`
	rec = callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, triggerBody, map[string]string{"Idempotency-Key": "req-123"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/workflow-api/runs = %d %s", rec.Code, rec.Body)
	}
	var runA apiRunSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &runA); err != nil {
		t.Fatal(err)
	}
	if runA.WorkflowID != wfA || runA.TriggerSource != "api" || runA.APIKeyID != keyAID || runA.APIKeyName != "ci-prod" {
		t.Fatalf("unexpected run summary: %+v", runA)
	}

	// Replay with same Idempotency-Key + same body returns 200 with same run ID and Idempotency-Replayed: true.
	rec2 := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, triggerBody, map[string]string{"Idempotency-Key": "req-123"})
	if rec2.Code != http.StatusOK || rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("idempotent replay = %d (header %q) %s", rec2.Code, rec2.Header().Get("Idempotency-Replayed"), rec2.Body)
	}
	var runA2 apiRunSummary
	_ = json.Unmarshal(rec2.Body.Bytes(), &runA2)
	if runA2.ID != runA.ID {
		t.Fatalf("replay created new run %s != %s", runA2.ID, runA.ID)
	}

	// Same Idempotency-Key with DIFFERENT body returns 409 Conflict.
	recConflict := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, `{"input":"Different contract"}`, map[string]string{"Idempotency-Key": "req-123"})
	if recConflict.Code != http.StatusConflict {
		t.Fatalf("idempotency mismatch = %d, want 409 (%s)", recConflict.Code, recConflict.Body)
	}

	// Concurrent duplicate POSTs with same Idempotency-Key create only one run.
	var wg sync.WaitGroup
	concIDs := make([]string, 6)
	concCodes := make([]int, 6)
	for i := range 6 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, `{"input":"Concurrent check"}`, map[string]string{"Idempotency-Key": "conc-1"})
			concCodes[idx] = r.Code
			var s apiRunSummary
			_ = json.Unmarshal(r.Body.Bytes(), &s)
			concIDs[idx] = s.ID
		}(i)
	}
	wg.Wait()
	for i := range 6 {
		if concCodes[i] != http.StatusCreated && concCodes[i] != http.StatusOK {
			t.Fatalf("concurrent[%d] code = %d", i, concCodes[i])
		}
		if concIDs[i] == "" || concIDs[i] != concIDs[0] {
			t.Fatalf("concurrent runs diverged: %v", concIDs)
		}
	}

	// Trigger a run on workflow B with key B.
	recB := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyB, `{"input":"Secret workflow B task"}`)
	if recB.Code != http.StatusCreated {
		t.Fatalf("POST wfB run = %d %s", recB.Code, recB.Body)
	}
	var runB apiRunSummary
	_ = json.Unmarshal(recB.Body.Bytes(), &runB)

	// 5. Strict workflow scoping: key A cannot see or control workflow B's run!
	if r := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID, keyA, ""); r.Code != http.StatusNotFound {
		t.Fatalf("keyA GET wfB run = %d, want 404", r.Code)
	}
	if r := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID+"/transcripts", keyA, ""); r.Code != http.StatusNotFound {
		t.Fatalf("keyA GET wfB transcripts = %d, want 404", r.Code)
	}
	if r := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID+"/stream", keyA, ""); r.Code != http.StatusNotFound {
		t.Fatalf("keyA GET wfB stream = %d, want 404", r.Code)
	}
	for _, action := range []string{"cancel", "reply", "input", "retry", "resume", "start"} {
		body := `{}`
		if action == "reply" || action == "input" {
			body = `{"text":"hi"}`
		}
		if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runB.ID+"/"+action, keyA, body); r.Code != http.StatusNotFound {
			t.Errorf("keyA POST wfB %s = %d, want 404", action, r.Code)
		}
	}

	// List runs with keyA only returns wfA runs, with pagination and status filtering.
	listRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs?limit=1", keyA, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list runs = %d %s", listRec.Code, listRec.Body)
	}
	var page1 struct {
		Runs       []apiRunSummary `json:"runs"`
		Total      int             `json:"total"`
		NextCursor string          `json:"nextCursor"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &page1)
	if page1.Total != 2 || len(page1.Runs) != 1 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v", page1)
	}
	if page1.Runs[0].WorkflowID != wfA {
		t.Fatalf("leaked workflow %s", page1.Runs[0].WorkflowID)
	}
	listRec2 := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs?limit=1&cursor="+page1.NextCursor, keyA, "")
	var page2 struct {
		Runs       []apiRunSummary `json:"runs"`
		Total      int             `json:"total"`
		NextCursor string          `json:"nextCursor"`
	}
	_ = json.Unmarshal(listRec2.Body.Bytes(), &page2)
	if len(page2.Runs) != 1 || page2.Runs[0].ID == page1.Runs[0].ID || page2.NextCursor != "" {
		t.Fatalf("page2 = %+v", page2)
	}
	if badStatus := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs?status=bogus", keyA, ""); badStatus.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d, want 400", badStatus.Code)
	}

	// 6. Simulate orchestrator starting for runA, reporting transcript, asking, replying, adding input, failing, retrying, and canceling!
	var launchCmdID string
	if err := d.Owner.QueryRow(ctx, `SELECT pending_command::text FROM workflow_runs WHERE id = $1`, runA.ID).Scan(&launchCmdID); err != nil {
		t.Fatal(err)
	}
	resJSON, _ := json.Marshal(hostclient.CommandResult{WorkspaceID: "ws-a", AgentID: "orch-a"})
	if err := reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: launchCmdID, Status: hostclient.CommandSucceeded, Result: resJSON}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := wfEng.CommandUpdated(ctx, launchCmdID); err != nil {
		t.Fatal(err)
	}
	rep := wfReport()
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, hostclient.Workspace{ID: "ws-a", Name: "ws-a", ProjectRoot: "/w/app"})
	rep.Paseo.Agents = append(rep.Paseo.Agents,
		hostclient.Agent{ID: "orch-a", WorkspaceID: "ws-a", Title: "Orchestrator", Provider: "claude", Model: "opus", Status: "running"},
		hostclient.Agent{ID: "worker-a", WorkspaceID: "ws-a", ParentID: "orch-a", Title: "Worker", Provider: "claude", Model: "opus", Status: "running"},
		hostclient.Agent{ID: "other-agent", WorkspaceID: "ws-1", Title: "Unrelated", Provider: "claude", Model: "opus", Status: "idle"},
	)
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "orch-a", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"Checking clauses."}`)},
			{SeqStart: 2, SeqEnd: 2, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"Clause 4 looks risky."}`)},
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Read run detail: includes attachment reference in input, steps, events, agents.
	detRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID, keyA, "")
	if detRec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d %s", detRec.Code, detRec.Body)
	}
	var det apiRunDetail
	_ = json.Unmarshal(detRec.Body.Bytes(), &det)
	if det.Status != workflow.Running || !strings.Contains(det.Input, "[attachment:") || len(det.Steps) != 2 || len(det.Agents) != 2 {
		t.Fatalf("unexpected run detail: status=%s steps=%d agents=%d input=%q", det.Status, len(det.Steps), len(det.Agents), det.Input)
	}

	// Read transcripts list & single agent transcript; unrelated agent returns 404.
	trListRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts", keyA, "")
	if trListRec.Code != http.StatusOK || !strings.Contains(trListRec.Body.String(), `"entryCount":2`) {
		t.Fatalf("GET transcripts = %d %s", trListRec.Code, trListRec.Body)
	}
	trRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/orch-a?after_seq=1", keyA, "")
	if trRec.Code != http.StatusOK || !strings.Contains(trRec.Body.String(), "Clause 4 looks risky.") || strings.Contains(trRec.Body.String(), "Checking clauses.") {
		t.Fatalf("GET transcript after_seq=1 = %d %s", trRec.Code, trRec.Body)
	}
	if otherTr := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/other-agent", keyA, ""); otherTr.Code != http.StatusNotFound {
		t.Fatalf("unrelated agent transcript = %d, want 404", otherTr.Code)
	}

	// Control: input (while running), ask -> reply (while waiting), fail -> retry, cancel!
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/input", keyA, `{"text":"Also check indemnity cap"}`); r.Code != http.StatusOK {
		t.Fatalf("POST input = %d %s", r.Code, r.Body)
	}
	if code, msg := move(t, h, hostKey, runA.ID, map[string]string{"move": "ask", "note": "Should we accept 2x cap?"}); code != http.StatusOK {
		t.Fatalf("move ask = %d %s", code, msg)
	}
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/reply", keyA, `{"text":"Yes, 2x is fine."}`); r.Code != http.StatusOK {
		t.Fatalf("POST reply = %d %s", r.Code, r.Body)
	}
	if code, msg := move(t, h, hostKey, runA.ID, map[string]string{"move": "fail", "why": "temporary error"}); code != http.StatusOK {
		t.Fatalf("move fail = %d %s", code, msg)
	}
	retryRec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/retry", keyA, `{}`)
	if retryRec.Code != http.StatusCreated {
		t.Fatalf("POST retry = %d %s", retryRec.Code, retryRec.Body)
	}
	var retried apiRunSummary
	_ = json.Unmarshal(retryRec.Body.Bytes(), &retried)
	if retried.RetriedFromRunID != runA.ID || retried.APIKeyID != keyAID {
		t.Fatalf("retried run = %+v", retried)
	}
	// Second retry returns 409.
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/retry", keyA, `{}`); r.Code != http.StatusConflict {
		t.Fatalf("second retry = %d, want 409", r.Code)
	}
	// Cancel the retried run.
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+retried.ID+"/cancel", keyA, `{}`); r.Code != http.StatusOK {
		t.Fatalf("POST cancel = %d %s", r.Code, r.Body)
	}

	// 7. UI pages render API trigger, API keys, and API-triggered run attribution.
	editorRec := call(t, h, http.MethodGet, "/workflows/"+wfA, "", sameOriginFetch)
	if editorRec.Code != http.StatusOK || !strings.Contains(editorRec.Body.String(), "ci-prod") {
		t.Fatalf("GET /workflows/%s = %d (missing key name)", wfA, editorRec.Code)
	}
	runPageRec := call(t, h, http.MethodGet, "/workflow-runs/"+runA.ID, "", sameOriginFetch)
	if runPageRec.Code != http.StatusOK || !strings.Contains(runPageRec.Body.String(), "triggered via <b>API</b>") || !strings.Contains(runPageRec.Body.String(), "ci-prod") {
		t.Fatalf("GET /workflow-runs/%s = %d (missing API attribution)", runA.ID, runPageRec.Code)
	}
	listPageRec := call(t, h, http.MethodGet, "/workflows", "", sameOriginFetch)
	if listPageRec.Code != http.StatusOK || !strings.Contains(listPageRec.Body.String(), "API trigger enabled") {
		t.Fatalf("GET /workflows = %d (missing API trigger badge)", listPageRec.Code)
	}

	// 8. Disabling API trigger makes keyA return 403; re-enabling makes it work; revoking makes it return 401.
	manualDef := apiWorkflowDef("host-1")
	manualDef.Trigger = workflow.TriggerManual
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfA, wfBody(t, "Agreement review", manualDef), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT manual = %d %s", rec.Code, rec.Body)
	}
	if rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs", keyA, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled trigger = %d, want 403", rec.Code)
	}
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfA, wfBody(t, "Agreement review", apiWorkflowDef("host-1")), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT api = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wfA+"/api-keys/"+keyAID, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE key = %d %s", rec.Code, rec.Body)
	}
	if rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs", keyA, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d, want 401", rec.Code)
	}

	// 9. SQL contract check passes.
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatalf("CheckContract: %v", err)
	}
}

type sseMessage struct {
	ID, Event, Data string
}

func readSSE(r *bufio.Reader) (sseMessage, error) {
	var msg sseMessage
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return msg, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if msg.Event != "" || msg.Data != "" || msg.ID != "" {
				return msg, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			msg.ID = v
		} else if v, ok := strings.CutPrefix(line, "event: "); ok {
			msg.Event = v
		} else if v, ok := strings.CutPrefix(line, "data: "); ok {
			msg.Data = v
		}
	}
}

func TestWorkflowAPIStreamRealtimeResumeAndRevocation(t *testing.T) {
	d := dbtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newServer(t, d, d.Owner, "test", func(c *Config) { c.Context = ctx })
	reg := fleet.New(d.Owner)
	wfEng := workflow.NewEngine(d.Owner, nil)

	hostKey := keyOf(t, h, "host-1")
	if err := reg.Record(ctx, wfReport(), time.Now()); err != nil {
		t.Fatal(err)
	}
	wfID := createWorkflow(t, h, apiWorkflowDef("host-1"))
	keyID, key := issueAPIKey(t, h, wfID, "stream-client")

	// Start a run via API.
	rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Stream this contract"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST run = %d %s", rec.Code, rec.Body)
	}
	var runA apiRunSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &runA)

	// Start orchestrator so we can stream live moves and transcripts.
	var launchCmdID string
	if err := d.Owner.QueryRow(ctx, `SELECT pending_command::text FROM workflow_runs WHERE id = $1`, runA.ID).Scan(&launchCmdID); err != nil {
		t.Fatal(err)
	}
	resJSON, _ := json.Marshal(hostclient.CommandResult{WorkspaceID: "ws-s", AgentID: "orch-s"})
	if err := reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: launchCmdID, Status: hostclient.CommandSucceeded, Result: resJSON}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := wfEng.CommandUpdated(ctx, launchCmdID); err != nil {
		t.Fatal(err)
	}
	rep := wfReport()
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, hostclient.Workspace{ID: "ws-s", Name: "ws-s", ProjectRoot: "/w/app"})
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "orch-s", WorkspaceID: "ws-s", Title: "Orchestrator", Provider: "claude", Model: "opus", Status: "running"})
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(anon(h))
	defer ts.Close()

	// 1. Open SSE stream, read initial snapshot + events, then push live transcript + move + revocation!
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+runA.ID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)

	// Read initial `run` snapshot and initial `run_event`s.
	msg, err := readSSE(reader)
	if err != nil || msg.Event != "run" {
		t.Fatalf("first SSE = (%+v, %v), want event=run", msg, err)
	}
	var lastEventID string
	for range 3 { // started, project, workspace
		m, err := readSSE(reader)
		if err != nil || m.Event != "run_event" {
			t.Fatalf("initial event = (%+v, %v)", m, err)
		}
		lastEventID = m.ID
	}

	// Push a live transcript batch and verify it arrives on the stream!
	if err := reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "orch-s", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"Live streaming line 1"}`)},
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	trMsg, err := readSSE(reader)
	if err != nil || trMsg.Event != "transcript" || !strings.Contains(trMsg.Data, "Live streaming line 1") {
		t.Fatalf("live transcript SSE = (%+v, %v)", trMsg, err)
	}

	// Advance the run (`next`) and verify live run_event + updated run snapshot arrive!
	if code, m := move(t, h, hostKey, runA.ID, map[string]string{"move": "next", "summary": "Contract checked"}); code != http.StatusOK {
		t.Fatalf("move next = %d %s", code, m)
	}
	evMsg, err := readSSE(reader)
	if err != nil || evMsg.Event != "run_event" || !strings.Contains(evMsg.Data, "Contract checked") {
		t.Fatalf("live next event = (%+v, %v)", evMsg, err)
	}
	nextID := evMsg.ID
	runMsg, err := readSSE(reader)
	if err != nil || runMsg.Event != "run" {
		t.Fatalf("live run update = (%+v, %v)", runMsg, err)
	}

	// Revoke the key while the stream is open: the stream must emit an error event and close!
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wfID+"/api-keys/"+keyID, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke key = %d %s", rec.Code, rec.Body)
	}
	errMsg, err := readSSE(reader)
	if err != nil || errMsg.Event != "error" || !strings.Contains(errMsg.Data, "revoked") {
		t.Fatalf("revocation SSE = (%+v, %v), want error with revoked", errMsg, err)
	}
	if _, err := readSSE(reader); err != io.EOF {
		t.Fatalf("expected stream EOF after revocation, got %v", err)
	}

	// 2. Resumability: issue a new key, finish the run, connect with Last-Event-ID = lastEventID,
	// verify we only get events AFTER lastEventID and then `done` + EOF!
	_, key2 := issueAPIKey(t, h, wfID, "stream-client-2")
	if code, m := move(t, h, hostKey, runA.ID, map[string]string{"move": "done", "summary": "All done"}); code != http.StatusOK {
		t.Fatalf("move done = %d %s", code, m)
	}
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+runA.ID+"/stream?include_transcripts=false", nil)
	req2.Header.Set("Authorization", "Bearer "+key2)
	req2.Header.Set("Last-Event-ID", lastEventID)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	r2 := bufio.NewReader(resp2.Body)
	var gotEvents []sseMessage
	for {
		m, err := readSSE(r2)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		gotEvents = append(gotEvents, m)
	}
	// Expect: run snapshot, next event (id == nextID), done event, SSE done.
	if len(gotEvents) != 4 || gotEvents[0].Event != "run" || gotEvents[1].ID != nextID || gotEvents[2].Event != "run_event" || gotEvents[3].Event != "done" {
		t.Fatalf("resumed SSE sequence = %+v", gotEvents)
	}
}

func TestWorkflowAPIDisableTriggerDuringStreamAndCredentialsRejected(t *testing.T) {
	d := dbtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newServer(t, d, d.Owner, "test", func(c *Config) { c.Context = ctx })
	reg := fleet.New(d.Owner)

	_ = keyOf(t, h, "host-1")
	if err := reg.Record(ctx, wfReport(), time.Now()); err != nil {
		t.Fatal(err)
	}
	wfID := createWorkflow(t, h, apiWorkflowDef("host-1"))
	_, key := issueAPIKey(t, h, wfID, "stream-disable-test")

	// Credential in input is rejected with 400 without leaking the secret.
	badRec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Use `+key+` to auth"}`)
	if badRec.Code != http.StatusBadRequest || strings.Contains(badRec.Body.String(), key) {
		t.Fatalf("credential in input = %d %s", badRec.Code, badRec.Body)
	}

	// Start a run, open SSE stream, then disable API trigger -> stream must close with error.
	rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Check vendor NDA"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST run = %d %s", rec.Code, rec.Body)
	}
	var runA apiRunSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &runA)

	ts := httptest.NewServer(anon(h))
	defer ts.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+runA.ID+"/stream?include_transcripts=false", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	// Drain initial run snapshot + started + project events.
	for range 3 {
		if _, err := readSSE(reader); err != nil {
			t.Fatal(err)
		}
	}

	// Switch workflow trigger to manual while stream is open.
	manualDef := apiWorkflowDef("host-1")
	manualDef.Trigger = workflow.TriggerManual
	if r := call(t, h, http.MethodPut, "/api/workflows/"+wfID, wfBody(t, "Agreement review", manualDef), sameOriginFetch); r.Code != http.StatusOK {
		t.Fatalf("PUT manual = %d %s", r.Code, r.Body)
	}
	errMsg, err := readSSE(reader)
	if err != nil || errMsg.Event != "error" || !strings.Contains(errMsg.Data, "disabled") {
		t.Fatalf("disable trigger SSE = (%+v, %v), want error mentioning disabled", errMsg, err)
	}
	if _, err := readSSE(reader); err != io.EOF {
		t.Fatalf("expected EOF after trigger disabled, got %v", err)
	}
}
