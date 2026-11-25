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
		Host:       host,
		Trigger:    workflow.TriggerManual,
		APITrigger: true,
		Project:    workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"},
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

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, wantCode int) string {
	t.Helper()
	if rec.Code != wantCode {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantCode, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (body: %s)", ct, rec.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("expected JSON {\"error\":...}, got %s", rec.Body)
	}
	return body["error"]
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

	// M3: trigger="api" is rejected; apiTrigger works alongside task (preserving default), email, voice_memo, and manual.
	badDef := apiWorkflowDef("host-1")
	badDef.Trigger = "api"
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Bad", badDef), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Fatalf("trigger=api = %d, want 400", rec.Code)
	}

	taskDef := apiWorkflowDef("host-1")
	taskDef.Trigger = workflow.TriggerTask
	taskDef.APITrigger = false
	wfTask := createWorkflow(t, h, taskDef)
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfTask+"/default", `{"default":true}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("set default = %d %s", rec.Code, rec.Body)
	}
	taskDef.APITrigger = true
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfTask, wfBody(t, "Default Task", taskDef), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("enable apiTrigger on default task = %d %s", rec.Code, rec.Body)
	}
	var isDefault bool
	if err := d.Owner.QueryRow(ctx, `SELECT is_default FROM workflows WHERE id = $1`, wfTask).Scan(&isDefault); err != nil || !isDefault {
		t.Fatalf("enabling apiTrigger cleared is_default: %v %v", isDefault, err)
	}

	emailDef := apiWorkflowDef("host-1")
	emailDef.Trigger = workflow.TriggerEmail
	emailDef.Email = &workflow.EmailFilter{From: []string{"sender@example.com"}}
	_ = createWorkflow(t, h, emailDef)

	vmDef := apiWorkflowDef("host-1")
	vmDef.Trigger = workflow.TriggerVoiceMemo
	vmDef.VoiceMemo = &workflow.VoiceMemoFilter{Title: "contracts"}
	_ = createWorkflow(t, h, vmDef)

	// Create two API-enabled workflows: A and B.
	wfA := createWorkflow(t, h, apiWorkflowDef("host-1"))
	wfB := createWorkflow(t, h, apiWorkflowDef("host-1"))
	keyAID, keyA := issueAPIKey(t, h, wfA, "ci-prod")
	_, keyB := issueAPIKey(t, h, wfB, "other-repo")

	// 1. Auth boundary: API key cannot reach any non-workflow-API route.
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
		assertJSONError(t, rec, tc.want)
	}
	if rec := call(t, h, http.MethodGet, "/api/workflow-api/workflow", "", sameOriginFetch); rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser cookie on /api/workflow-api/workflow = %d, want 401", rec.Code)
	}

	// 3. GET /api/workflow-api/workflow returns scoped workflow metadata including apiTrigger=true.
	rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/workflow", keyA, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"apiTrigger":true`) {
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

	// Same Idempotency-Key with DIFFERENT body returns 409 Conflict JSON.
	recConflict := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, `{"input":"Different contract"}`, map[string]string{"Idempotency-Key": "req-123"})
	assertJSONError(t, recConflict, http.StatusConflict)

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

	// 5. Strict workflow scoping: key A cannot see or control workflow B's run.
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID, keyA, ""), http.StatusNotFound)
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID+"/transcripts", keyA, ""), http.StatusNotFound)
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runB.ID+"/stream", keyA, ""), http.StatusNotFound)
	for _, action := range []string{"cancel", "reply", "input", "retry", "resume", "start"} {
		body := `{}`
		if action == "reply" || action == "input" {
			body = `{"text":"hi"}`
		}
		assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runB.ID+"/"+action, keyA, body), http.StatusNotFound)
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
	if page1.Total != 2 || len(page1.Runs) != 1 || page1.NextCursor == "" || page1.Runs[0].WorkflowID != wfA {
		t.Fatalf("page1 = %+v", page1)
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
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs?status=bogus", keyA, ""), http.StatusBadRequest)

	// 6. Simulate orchestrator starting for runA, moving to Sign-off step, asking, API-key replying (M4!),
	// adding input, failing, retrying, and canceling!
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

	// Read run detail & transcripts; unrelated agent returns 404.
	detRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID, keyA, "")
	if detRec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d %s", detRec.Code, detRec.Body)
	}
	var det apiRunDetail
	_ = json.Unmarshal(detRec.Body.Bytes(), &det)
	if det.Status != workflow.Running || !strings.Contains(det.Input, "[attachment:") || len(det.Steps) != 2 || len(det.Agents) != 2 {
		t.Fatalf("unexpected run detail: status=%s steps=%d agents=%d input=%q", det.Status, len(det.Steps), len(det.Agents), det.Input)
	}
	trListRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts", keyA, "")
	if trListRec.Code != http.StatusOK || !strings.Contains(trListRec.Body.String(), `"entryCount":2`) {
		t.Fatalf("GET transcripts = %d %s", trListRec.Code, trListRec.Body)
	}
	trRec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/orch-a?after_seq=1", keyA, "")
	if trRec.Code != http.StatusOK || !strings.Contains(trRec.Body.String(), "Clause 4 looks risky.") || strings.Contains(trRec.Body.String(), "Checking clauses.") {
		t.Fatalf("GET transcript after_seq=1 = %d %s", trRec.Code, trRec.Body)
	}
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/other-agent", keyA, ""), http.StatusNotFound)

	// Refused reply (run is not waiting) with inline attachment must leave zero orphaned attachments.
	var attBefore int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM attachments`).Scan(&attBefore)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/reply", keyA,
		`{"text":"too early","attachments":[{"name":"note.txt","data":"`+attData+`"}]}`), http.StatusConflict)
	var attAfter int
	_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM attachments`).Scan(&attAfter)
	if attAfter != attBefore {
		t.Fatalf("orphaned attachment after 409 reply: before=%d after=%d", attBefore, attAfter)
	}

	// M4: Send input while running, advance to Sign-off step, ask, reply via API key!
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/input", keyA, `{"text":"Also check indemnity cap"}`); r.Code != http.StatusOK {
		t.Fatalf("POST input = %d %s", r.Code, r.Body)
	}
	if code, msg := move(t, h, hostKey, runA.ID, map[string]string{"move": "next", "summary": "Checked"}); code != http.StatusOK {
		t.Fatalf("move next = %d %s", code, msg)
	}
	if code, msg := move(t, h, hostKey, runA.ID, map[string]string{"move": "ask", "note": "Approve sign-off?"}); code != http.StatusOK {
		t.Fatalf("move ask = %d %s", code, msg)
	}
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/reply", keyA, `{"text":"Approved, proceed."}`); r.Code != http.StatusOK {
		t.Fatalf("POST reply = %d %s", r.Code, r.Body)
	}

	// Verify M4: orchestrator command names the API key (never "Zach replied") AND includes non-Zach sign-off warning!
	var replyMsg string
	if err := d.Owner.QueryRow(ctx, `SELECT args->>'text' FROM commands WHERE host='host-1' AND kind='send_message' ORDER BY created_at DESC LIMIT 1`).Scan(&replyMsg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(replyMsg, "Zach replied") || !strings.Contains(replyMsg, `API key "ci-prod"`) || !strings.Contains(replyMsg, "not Zach's approval") {
		t.Fatalf("sign-off reply message missing API key attribution: %q", replyMsg)
	}
	// Verify M4: workflow_run_events.api_key_id is set on reply and input events!
	var replyKeyID, inputKeyID string
	if err := d.Owner.QueryRow(ctx, `SELECT coalesce(api_key_id::text,'') FROM workflow_run_events WHERE run_id=$1 AND kind='reply'`, runA.ID).Scan(&replyKeyID); err != nil || replyKeyID != keyAID {
		t.Fatalf("reply event api_key_id = %q, want %q (err=%v)", replyKeyID, keyAID, err)
	}
	if err := d.Owner.QueryRow(ctx, `SELECT coalesce(api_key_id::text,'') FROM workflow_run_events WHERE run_id=$1 AND kind='input'`, runA.ID).Scan(&inputKeyID); err != nil || inputKeyID != keyAID {
		t.Fatalf("input event api_key_id = %q, want %q (err=%v)", inputKeyID, keyAID, err)
	}

	// Fail, retry, and cancel via API key; check cancel attribution on event and move refusal!
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
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/retry", keyA, `{}`), http.StatusConflict)
	if r := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+retried.ID+"/cancel", keyA, `{}`); r.Code != http.StatusOK {
		t.Fatalf("POST cancel = %d %s", r.Code, r.Body)
	}
	if code, msg := move(t, h, hostKey, retried.ID, map[string]string{"move": "next", "summary": "late"}); code != http.StatusConflict || !strings.Contains(msg, `API key "ci-prod" canceled`) || strings.Contains(msg, "Zach canceled") {
		t.Fatalf("move on API-canceled run = (%d, %q)", code, msg)
	}

	// 7. UI pages render API trigger checkbox, API keys, and API-key reply/cancel attribution (never "Zach replied").
	editorRec := call(t, h, http.MethodGet, "/workflows/"+wfA, "", sameOriginFetch)
	if editorRec.Code != http.StatusOK || !strings.Contains(editorRec.Body.String(), "ci-prod") || !strings.Contains(editorRec.Body.String(), "Allow triggering via API") {
		t.Fatalf("GET /workflows/%s = %d (missing API UI)", wfA, editorRec.Code)
	}
	runPageRec := call(t, h, http.MethodGet, "/workflow-runs/"+runA.ID, "", sameOriginFetch)
	if runPageRec.Code != http.StatusOK || !strings.Contains(runPageRec.Body.String(), "API key ci-prod replied:") || strings.Contains(runPageRec.Body.String(), "Zach replied:") {
		t.Fatalf("GET /workflow-runs/%s = %d (wrong reply attribution)", runA.ID, runPageRec.Code)
	}
	canceledPageRec := call(t, h, http.MethodGet, "/workflow-runs/"+retried.ID, "", sameOriginFetch)
	if canceledPageRec.Code != http.StatusOK || !strings.Contains(canceledPageRec.Body.String(), "Canceled by API key ci-prod") {
		t.Fatalf("GET /workflow-runs/%s = %d (missing cancel key attribution)", retried.ID, canceledPageRec.Code)
	}

	// 8. M3: Disabling apiTrigger blocks new starts and retries (403), while reads and existing-run controls still work!
	disabledDef := apiWorkflowDef("host-1")
	disabledDef.APITrigger = false
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfA, wfBody(t, "Agreement review", disabledDef), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT apiTrigger=false = %d %s", rec.Code, rec.Body)
	}
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", keyA, `{"input":"blocked"}`), http.StatusForbidden)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/retry", keyA, `{}`), http.StatusForbidden)
	if rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs", keyA, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /runs while apiTrigger=false = %d, want 200", rec.Code)
	}
	if rec := callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID, keyA, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /runs/{id} while apiTrigger=false = %d, want 200", rec.Code)
	}

	// Revoking the key cuts off ALL routes (401).
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wfA+"/api-keys/"+keyAID, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE key = %d %s", rec.Code, rec.Body)
	}
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs", keyA, ""), http.StatusUnauthorized)

	// 9. SQL contract check passes.
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatalf("CheckContract: %v", err)
	}
}

func TestWorkflowAPIInputValidationAndSizeLimits(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "test")
	reg := fleet.New(d.Owner)
	ctx := context.Background()
	_ = keyOf(t, h, "host-1")
	if err := reg.Record(ctx, wfReport(), time.Now()); err != nil {
		t.Fatal(err)
	}
	wfID := createWorkflow(t, h, apiWorkflowDef("host-1"))
	_, key := issueAPIKey(t, h, wfID, "validator")

	// Start a valid run so we can test reply/input/transcript bad-input cases against it.
	rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Valid run"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST valid run = %d %s", rec.Code, rec.Body)
	}
	var runA apiRunSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &runA)

	// M1: > 1 MiB input on trigger -> 413 JSON.
	bigText := strings.Repeat("a", (1<<20)+1)
	bigPayload, _ := json.Marshal(map[string]string{"input": bigText})
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, string(bigPayload)), http.StatusRequestEntityTooLarge)

	// M1: > 1 MiB text on reply and input -> 413 JSON.
	bigReply, _ := json.Marshal(map[string]string{"text": bigText})
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/reply", key, string(bigReply)), http.StatusRequestEntityTooLarge)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/input", key, string(bigReply)), http.StatusRequestEntityTooLarge)

	// M1: host MaxFrame guard in fleet.EnqueueTx refuses an oversized command frame.
	hugeArgs, _ := json.Marshal(hostclient.AgentArgs{AgentID: "a1", Text: strings.Repeat("x", hostclient.MaxFrame)})
	if _, err := reg.Enqueue(ctx, "host-1", hostclient.SendMessageKind, hugeArgs, time.Minute, time.Now()); err == nil {
		t.Fatal("expected fleet.Enqueue to refuse command exceeding hostclient.MaxFrame")
	}

	// M5: NUL bytes in trigger input, reply text, and input text -> 400 JSON.
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"a\u0000b"}`), http.StatusBadRequest)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/reply", key, `{"text":"a\u0000b"}`), http.StatusBadRequest)
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs/"+runA.ID+"/input", key, `{"text":"a\u0000b"}`), http.StatusBadRequest)

	// M5: Credential in input -> 400 JSON without disclosing the key.
	credRec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Use `+key+` here"}`)
	credErr := assertJSONError(t, credRec, http.StatusBadRequest)
	if strings.Contains(credErr, key) {
		t.Fatalf("error disclosed API key: %q", credErr)
	}

	// M5: Wrong Content-Type -> 415 JSON (not plain text!).
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"hi"}`, map[string]string{"Content-Type": "text/plain"}), http.StatusUnsupportedMediaType)

	// M5: Trailing JSON value -> 400 JSON.
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"first"}{"input":"second"}`), http.StatusBadRequest)

	// M5: Over-long Idempotency-Key -> 400 JSON.
	assertJSONError(t, callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"hi"}`, map[string]string{"Idempotency-Key": strings.Repeat("k", 256)}), http.StatusBadRequest)

	// M5: Int32 overflow on after_seq and from_seq -> 400 JSON.
	var launchCmdID string
	_ = d.Owner.QueryRow(ctx, `SELECT pending_command::text FROM workflow_runs WHERE id = $1`, runA.ID).Scan(&launchCmdID)
	resJSON, _ := json.Marshal(hostclient.CommandResult{WorkspaceID: "ws-v", AgentID: "orch-v"})
	_ = reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: launchCmdID, Status: hostclient.CommandSucceeded, Result: resJSON}, time.Now())
	_ = workflow.NewEngine(d.Owner, nil).CommandUpdated(ctx, launchCmdID)
	rep := wfReport()
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, hostclient.Workspace{ID: "ws-v", Name: "ws-v", ProjectRoot: "/w/app"})
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "orch-v", WorkspaceID: "ws-v", Title: "Orchestrator", Provider: "claude", Model: "opus", Status: "running"})
	_ = reg.Record(ctx, rep, time.Now())
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/orch-v?after_seq=99999999999999", key, ""), http.StatusBadRequest)
	assertJSONError(t, callAPI(t, h, http.MethodGet, "/api/workflow-api/runs/"+runA.ID+"/transcripts/orch-v?from_seq=99999999999999", key, ""), http.StatusBadRequest)
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

// TestWorkflowAPIStreamLiveAgentsWorkersGraceAndResume covers M2a, M2b, M2c, M2d, and key revocation.
func TestWorkflowAPIStreamLiveAgentsWorkersGraceAndResume(t *testing.T) {
	d := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	// Launch completes, setting orchestrator_id="orch-s", BEFORE the host reports the agent (M2a order!).
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

	ts := httptest.NewServer(anon(h))
	defer ts.Close()

	// Open SSE stream BEFORE orch-s is reported by host (M2a).
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+runA.ID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	// Drain initial run + 3 initial events (started, project, workspace).
	if m, err := readSSE(reader); err != nil || m.Event != "run" {
		t.Fatalf("first SSE = (%+v, %v)", m, err)
	}
	for range 3 {
		if m, err := readSSE(reader); err != nil || m.Event != "run_event" {
			t.Fatalf("initial event = (%+v, %v)", m, err)
		}
	}

	// M2a: Host now reports orch-s, then stores its transcript. No unrelated move happens!
	rep := wfReport()
	rep.Paseo.Workspaces = append(rep.Paseo.Workspaces, hostclient.Workspace{ID: "ws-s", Name: "ws-s", ProjectRoot: "/w/app"})
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "orch-s", WorkspaceID: "ws-s", Title: "Orchestrator", Provider: "claude", Model: "opus", Status: "running"})
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "orch-s", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"Orchestrator live line"}`)},
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	trMsg, err := readSSE(reader)
	if err != nil || trMsg.Event != "transcript" || !strings.Contains(trMsg.Data, "Orchestrator live line") || trMsg.ID == "" {
		t.Fatalf("M2a orchestrator transcript = (%+v, %v)", trMsg, err)
	}

	// M2b: Host reports a new worker sub-agent while orchestrator is idle, then worker writes transcript!
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "worker-s", WorkspaceID: "ws-s", ParentID: "orch-s", Title: "Worker", Provider: "claude", Model: "opus", Status: "running"})
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "worker-s", Epoch: "wep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"Worker live line"}`)},
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	wMsg, err := readSSE(reader)
	if err != nil || wMsg.Event != "transcript" || !strings.Contains(wMsg.Data, "Worker live line") || wMsg.ID == "" {
		t.Fatalf("M2b worker transcript = (%+v, %v)", wMsg, err)
	}
	resumeCursor := wMsg.ID

	// M2d: Reconnect a second stream using Last-Event-ID = resumeCursor -> must NOT resend earlier transcripts!
	// Store a new entry seq=2 on orch-s first, then reconnect with resumeCursor.
	if err := reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "orch-s", Epoch: "ep1", Replace: false,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 2, SeqEnd: 2, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"After resume line"}`)},
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+runA.ID+"/stream", nil)
	req2.Header.Set("Authorization", "Bearer "+key)
	req2.Header.Set("Last-Event-ID", resumeCursor)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	r2 := bufio.NewReader(resp2.Body)
	if m, err := readSSE(r2); err != nil || m.Event != "run" {
		t.Fatalf("M2d first SSE = (%+v, %v)", m, err)
	}
	m2, err := readSSE(r2)
	if err != nil || m2.Event != "transcript" || !strings.Contains(m2.Data, "After resume line") || strings.Contains(m2.Data, "Orchestrator live line") || strings.Contains(m2.Data, "Worker live line") {
		t.Fatalf("M2d resumed transcript = (%+v, %v), want only 'After resume line'", m2, err)
	}

	// M2c: Finish the run (`done`), then 150ms later store "FINAL VERDICT" transcript -> stream must deliver it BEFORE `done`!
	if code, msg := move(t, h, hostKey, runA.ID, map[string]string{"move": "done", "summary": "All done"}); code != http.StatusOK {
		t.Fatalf("move done = %d %s", code, msg)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
			Host: "host-1", AgentID: "orch-s", Epoch: "ep1", Replace: false,
			Entries: []hostclient.TranscriptEntry{
				{SeqStart: 3, SeqEnd: 3, Provider: "claude", Item: json.RawMessage(`{"type":"assistant_message","text":"FINAL VERDICT"}`)},
			},
		}, time.Now())
	}()
	sawFinal := false
	sawDone := false
	for {
		m, err := readSSE(r2)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if m.Event == "transcript" && strings.Contains(m.Data, "FINAL VERDICT") {
			sawFinal = true
		}
		if m.Event == "done" {
			sawDone = true
		}
	}
	if !sawFinal || !sawDone {
		t.Fatalf("M2c: sawFinal=%v sawDone=%v, want both true", sawFinal, sawDone)
	}

	// Revocation closes an open stream immediately with event: error!
	rec3 := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", key, `{"input":"Revoke test run"}`)
	var run3 apiRunSummary
	_ = json.Unmarshal(rec3.Body.Bytes(), &run3)
	req3, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/workflow-api/runs/"+run3.ID+"/stream?include_transcripts=false", nil)
	req3.Header.Set("Authorization", "Bearer "+key)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	r3 := bufio.NewReader(resp3.Body)
	for range 3 { // run, started, project
		if _, err := readSSE(r3); err != nil {
			t.Fatal(err)
		}
	}
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wfID+"/api-keys/"+keyID, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	errMsg, err := readSSE(r3)
	if err != nil || errMsg.Event != "error" || !strings.Contains(errMsg.Data, "revoked") {
		t.Fatalf("revocation SSE = (%+v, %v)", errMsg, err)
	}
	if _, err := readSSE(r3); err != io.EOF {
		t.Fatalf("expected EOF after revocation, got %v", err)
	}
}
