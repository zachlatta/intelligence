package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/vaultwarden"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type fakeSecretStore struct {
	mu         sync.Mutex
	collection string
	stored     []vaultwarden.StoreRequest
	failErr    error
	nextSeq    int
}

func (f *fakeSecretStore) TargetCollection() string {
	if f.collection != "" {
		return f.collection
	}
	return vaultwarden.DefaultCollection
}

func (f *fakeSecretStore) StoreSecret(_ context.Context, req vaultwarden.StoreRequest) (vaultwarden.StoredItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return vaultwarden.StoredItem{}, f.failErr
	}
	f.nextSeq++
	f.stored = append(f.stored, req)
	return vaultwarden.StoredItem{
		ID:         fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", f.nextSeq),
		Name:       req.Name,
		Collection: f.TargetCollection(),
	}, nil
}

func dbServerWithSecrets(t *testing.T, store workflow.SecretStore) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return newServer(t, d, d.Owner, "test", func(c *Config) { c.Secrets = store }), d
}

func setupRunningWorkflow(t *testing.T, h http.Handler, d dbtest.DB) (hostKey, runID string) {
	t.Helper()
	sendReport(t, h, wfReport())
	hostKey = keyOf(t, h, "host-1")
	def := workflow.Starting
	def.Trigger = workflow.TriggerManual
	def.Host = "host-1"
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{{ID: "do", Kind: workflow.KindStep, Title: "Do the work", Instructions: "work"}}
	wfID := createWorkflow(t, h, def)
	runID = startRun(t, h, wfID, "Configure billing integration")
	if _, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET status = 'running', step = 0, orchestrator_id = 'orch-1', workspace_id = 'ws-1' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	return hostKey, runID
}

func moveAny(t *testing.T, h http.Handler, key, run string, m any) (int, string) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+run+"/moves", string(body), map[string]string{"Authorization": "Bearer " + key})
	var out struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Message == "" {
		out.Message = rec.Body.String()
	}
	return rec.Code, out.Message
}

func TestWorkflowSecretAskAndSubmitEndToEndNeverLeaksValue(t *testing.T) {
	t.Parallel()
	store := &fakeSecretStore{}
	h, d := dbServerWithSecrets(t, store)
	hostKey, runID := setupRunningWorkflow(t, h, d)

	// 1. Orchestrator asks for two secrets.
	code, msg := moveAny(t, h, hostKey, runID, map[string]any{
		"move": "ask",
		"note": "I need the Stripe API key and webhook signing secret to finish setup.",
		"secrets": []map[string]string{
			{"name": "Stripe API key", "purpose": "Read invoices from Stripe"},
			{"name": "Stripe webhook secret", "purpose": "Verify webhook signatures", "username": "stripe-webhook"},
		},
	})
	if code != http.StatusOK || !strings.Contains(msg, "Asked Zach") {
		t.Fatalf("ask move = %d %q", code, msg)
	}

	// 2. Ask page shows password inputs (type=password, autocomplete=off) for each secret.
	page := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("GET run page = %d", page.Code)
	}
	html := page.Body.String()
	for _, want := range []string{
		`type="password"`,
		`autocomplete="off"`,
		`Stripe API key`,
		`Read invoices from Stripe`,
		`Stripe webhook secret`,
		`stripe-webhook`,
		`AI Agents Shared Passwords`,
		`id="store-secrets"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("run page missing %q", want)
		}
	}

	// 3. Read pending secret request IDs via read-only /api/sql (verifying SQL contract & RLS).
	sqlCode, sqlRes := sqlPost(t, h, q(`SELECT id::text, name, purpose, username, status FROM workflow_run_secret_requests WHERE run_id = '`+runID+`' ORDER BY position`))
	if sqlCode != http.StatusOK || len(sqlRes.Rows) != 2 {
		t.Fatalf("SQL secret requests = %d %+v", sqlCode, sqlRes)
	}
	id1 := sqlRes.Rows[0][0].(string)
	id2 := sqlRes.Rows[1][0].(string)

	// Another account cannot see these rows via /api/sql.
	signUpRec := call(t, anon(h), http.MethodPost, "/api/accounts", `{"username":"other","password":"correct-horse-battery"}`, sameOriginFetch)
	if signUpRec.Code != http.StatusCreated {
		t.Fatalf("signup other = %d %s", signUpRec.Code, signUpRec.Body.String())
	}
	otherCookie := signUpRec.Result().Cookies()[0]
	otherSQLRec := call(t, anon(h), http.MethodPost, "/api/sql", q(`SELECT id FROM workflow_run_secret_requests`), map[string]string{
		"Origin": "http://example.com",
		"Cookie": otherCookie.Name + "=" + otherCookie.Value,
	})
	var otherRes struct {
		Rows [][]any `json:"rows"`
	}
	_ = json.Unmarshal(otherSQLRec.Body.Bytes(), &otherRes)
	if otherSQLRec.Code != http.StatusOK || len(otherRes.Rows) != 0 {
		t.Fatalf("RLS leak to other account: %d %+v", otherSQLRec.Code, otherRes)
	}

	secret1 := "sk_live_NEVER_LEAK_WEB_111111111"
	secret2 := "whsec_NEVER_LEAK_WEB_222222222"

	// 4. Refusal if secret value is also pasted into the plain text note.
	badBody, _ := json.Marshal(map[string]any{
		"secrets": []map[string]string{
			{"id": id1, "value": secret1},
			{"id": id2, "value": secret2},
		},
		"text": "Also pasting " + secret1,
	})
	badRec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(badBody), sameOriginFetch)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when note contains secret, got %d %s", badRec.Code, badRec.Body.String())
	}
	if strings.Contains(badRec.Body.String(), secret1) {
		t.Fatalf("error response leaked secret value: %s", badRec.Body.String())
	}

	// 5. Submit secrets via JSON API route.
	goodBody, _ := json.Marshal(map[string]any{
		"secrets": []map[string]string{
			{"id": id1, "value": secret1},
			{"id": id2, "value": secret2},
		},
		"text": "Use test mode first.",
	})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(goodBody), sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /secrets = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("expected Cache-Control: no-store, got %q", rec.Header().Get("Cache-Control"))
	}

	// 6. Verify Vaultwarden received both secrets.
	if len(store.stored) != 2 || store.stored[0].Password != secret1 || store.stored[1].Password != secret2 || store.stored[1].Username != "stripe-webhook" {
		t.Fatalf("unexpected Vaultwarden writes: %+v", store.stored)
	}

	// 7. Verify run resumed and orchestrator command has Vaultwarden fetch instructions (not the values).
	runRow := readRun(t, h, runID)
	if runRow["status"] != workflow.Running {
		t.Fatalf("expected running after secret submit, got %v", runRow["status"])
	}
	var cmdText string
	if err := d.Owner.QueryRow(context.Background(), `SELECT args->>'text' FROM commands WHERE kind = 'send_message' ORDER BY created_at DESC LIMIT 1`).Scan(&cmdText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdText, "vaultwarden-agent get password aaaaaaaa-aaaa-4aaa-8aaa-000000000001") ||
		!strings.Contains(cmdText, "Stripe API key") ||
		!strings.Contains(cmdText, "Use test mode first.") {
		t.Fatalf("orchestrator message missing Vaultwarden fetch guidance: %q", cmdText)
	}

	// 8. Scan EVERY stored event, command, run row, secret request row, and rendered HTML for both secret values.
	afterPage := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil)
	if afterPage.Code != http.StatusOK {
		t.Fatalf("GET run page after submit = %d", afterPage.Code)
	}
	afterHTML := afterPage.Body.String()
	for _, needle := range []string{secret1, secret2} {
		if strings.Contains(afterHTML, needle) {
			t.Fatalf("secret %q leaked into rendered run page HTML", needle)
		}
		if strings.Contains(cmdText, needle) {
			t.Fatalf("secret %q leaked into orchestrator message", needle)
		}
		var leaks int
		if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM workflow_run_events WHERE run_id = $1 AND text LIKE '%' || $2 || '%'`, runID, needle).Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks > 0 {
			t.Fatalf("secret %q leaked into workflow_run_events", needle)
		}
		if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM commands WHERE args::text LIKE '%' || $1 || '%'`, needle).Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks > 0 {
			t.Fatalf("secret %q leaked into commands", needle)
		}
		if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM workflow_runs WHERE id = $1 AND (note LIKE '%' || $2 || '%' OR error LIKE '%' || $2 || '%' OR input LIKE '%' || $2 || '%')`, runID, needle).Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks > 0 {
			t.Fatalf("secret %q leaked into workflow_runs", needle)
		}
		if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM workflow_run_secret_requests WHERE run_id = $1 AND (name LIKE '%' || $2 || '%' OR purpose LIKE '%' || $2 || '%' OR username LIKE '%' || $2 || '%' OR vault_item_id LIKE '%' || $2 || '%' OR vault_item_name LIKE '%' || $2 || '%')`, runID, needle).Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks > 0 {
			t.Fatalf("secret %q leaked into workflow_run_secret_requests", needle)
		}
	}

	// 9. Verify stored metadata is visible through read-only SQL.
	sqlCode, sqlRes = sqlPost(t, h, q(`SELECT status, vault_item_id, vault_item_name, stored_by FROM workflow_run_secret_requests WHERE run_id = '`+runID+`' ORDER BY position`))
	if sqlCode != http.StatusOK || len(sqlRes.Rows) != 2 || sqlRes.Rows[0][0] != "stored" || sqlRes.Rows[0][3] == "" {
		t.Fatalf("stored SQL metadata = %d %+v", sqlCode, sqlRes)
	}
}

func TestWorkflowSecretUnconfiguredAndUpstreamFailure(t *testing.T) {
	t.Parallel()
	// Case A: Vaultwarden not configured (Secrets == nil).
	h, d := dbServerWithSecrets(t, nil)
	hostKey, runID := setupRunningWorkflow(t, h, d)

	code, _ := moveAny(t, h, hostKey, runID, map[string]any{
		"move":    "ask",
		"note":    "Need API key",
		"secrets": []map[string]string{{"name": "Demo API key", "purpose": "Testing"}},
	})
	if code != http.StatusOK {
		t.Fatalf("ask move = %d", code)
	}
	page := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil)
	if !strings.Contains(page.Body.String(), "Vaultwarden is not configured") {
		t.Fatalf("expected unconfigured warning on run page")
	}

	var reqID string
	if err := d.Owner.QueryRow(context.Background(), `SELECT id::text FROM workflow_run_secret_requests WHERE run_id = $1`, runID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	secretVal := "never-persist-unconfigured-999"
	body, _ := json.Marshal(map[string]any{
		"secrets": []map[string]string{{"id": reqID, "value": secretVal}},
	})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(body), sameOriginFetch)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when unconfigured, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretVal) {
		t.Fatalf("503 response leaked secret value")
	}
	if readRun(t, h, runID)["status"] != workflow.Waiting {
		t.Fatalf("run should stay waiting when Vaultwarden is unconfigured")
	}

	// Plain reply still works and dismisses the pending secret request.
	replyRec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/reply", `{"text":"Skip this key for now."}`, sameOriginFetch)
	if replyRec.Code != http.StatusNoContent {
		t.Fatalf("plain reply = %d %s", replyRec.Code, replyRec.Body.String())
	}
	var status string
	if err := d.Owner.QueryRow(context.Background(), `SELECT status FROM workflow_run_secret_requests WHERE id = $1`, reqID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dismissed" {
		t.Fatalf("expected dismissed after plain reply, got %q", status)
	}

	// Case B: Upstream Vaultwarden error returns 502 and never leaks secret value.
	badVal := "super-secret-upstream-fail-777"
	failStore := &fakeSecretStore{failErr: errors.New("upstream failed with " + badVal)}
	h2, d2 := dbServerWithSecrets(t, failStore)
	hostKey2, runID2 := setupRunningWorkflow(t, h2, d2)
	moveAny(t, h2, hostKey2, runID2, map[string]any{
		"move":    "ask",
		"note":    "Need token",
		"secrets": []map[string]string{{"name": "Deploy token"}},
	})
	var reqID2 string
	if err := d2.Owner.QueryRow(context.Background(), `SELECT id::text FROM workflow_run_secret_requests WHERE run_id = $1`, runID2).Scan(&reqID2); err != nil {
		t.Fatal(err)
	}
	body2, _ := json.Marshal(map[string]any{
		"secrets": []map[string]string{{"id": reqID2, "value": badVal}},
	})
	rec2 := call(t, h2, http.MethodPost, "/api/workflow-runs/"+runID2+"/secrets", string(body2), sameOriginFetch)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on upstream failure, got %d %s", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), badVal) {
		t.Fatalf("502 response leaked secret value: %s", rec2.Body.String())
	}
}
