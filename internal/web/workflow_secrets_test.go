package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/secretbox"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func testSecretBox(t *testing.T) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dbServerWithSecrets(t *testing.T, box *secretbox.Box) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	return newServer(t, d, d.Owner, "test", func(c *Config) { c.SecretBox = box }), d
}

func setupRunningWorkflow(t *testing.T, h http.Handler, d dbtest.DB) (hostKey, runID string) {
	t.Helper()
	return setupRunningWorkflowWith(t, h, d, workflow.Step{ID: "do", Kind: workflow.KindStep, Title: "Do the work", Instructions: "work"})
}

func setupRunningWorkflowWith(t *testing.T, h http.Handler, d dbtest.DB, step workflow.Step) (hostKey, runID string) {
	t.Helper()
	sendReport(t, h, wfReport())
	hostKey = keyOf(t, h, "host-1")
	def := workflow.Starting
	def.Trigger = workflow.TriggerManual
	def.Host = "host-1"
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{step}
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

// fetchSecretAs is `intelligence secret get` on the host whose key is key.
func fetchSecretAs(t *testing.T, h http.Handler, key, run, name string) (int, string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+run+"/secrets/fetch", string(body), map[string]string{"Authorization": "Bearer " + key})
	var out struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code == http.StatusOK && rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("fetch Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	return rec.Code, out.Value, out.Message
}

func secretIDs(t *testing.T, d dbtest.DB, runID string) []string {
	t.Helper()
	rows, err := d.Owner.Query(context.Background(), `SELECT id::text FROM workflow_run_secrets WHERE run_id = $1 ORDER BY ask_event_id, position`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// otherAccount signs up "other" with its own host, workflow and running run.
func otherAccount(t *testing.T, h http.Handler, d dbtest.DB) (headers map[string]string, hostKey, runID string) {
	t.Helper()
	signUpRec := call(t, anon(h), http.MethodPost, "/api/accounts", `{"username":"other","password":"correct-horse-battery"}`, sameOriginFetch)
	if signUpRec.Code != http.StatusCreated {
		t.Fatalf("signup other = %d %s", signUpRec.Code, signUpRec.Body.String())
	}
	c := signUpRec.Result().Cookies()[0]
	headers = map[string]string{"Origin": "http://example.com", "Cookie": c.Name + "=" + c.Value}
	enrRec := call(t, anon(h), http.MethodPost, "/api/host-enrollments", `{"host":"other-host"}`, nil)
	if enrRec.Code != http.StatusCreated {
		t.Fatalf("enroll other-host = %d %s", enrRec.Code, enrRec.Body.String())
	}
	var enr struct{ ID, Code, Secret string }
	_ = json.Unmarshal(enrRec.Body.Bytes(), &enr)
	if rec := call(t, anon(h), http.MethodPost, "/api/host-enrollments/claim", fmt.Sprintf(`{"code":%q}`, enr.Code), headers); rec.Code != http.StatusOK {
		t.Fatalf("claim other-host = %d %s", rec.Code, rec.Body.String())
	}
	keyRec := call(t, anon(h), http.MethodPost, "/api/host-enrollments/"+enr.ID+"/key", `{}`, map[string]string{"Authorization": "Bearer " + enr.Secret})
	if keyRec.Code != http.StatusOK {
		t.Fatalf("collect key other-host = %d %s", keyRec.Code, keyRec.Body.String())
	}
	var keyOut struct{ Key string }
	_ = json.Unmarshal(keyRec.Body.Bytes(), &keyOut)
	if _, err := d.Owner.Exec(context.Background(), `INSERT INTO hosts (name, connected, last_seen) VALUES ('other-host', true, now())`); err != nil {
		t.Fatal(err)
	}
	def := workflow.Starting
	def.Trigger = workflow.TriggerManual
	def.Host = "other-host"
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = []workflow.Step{{ID: "do", Kind: workflow.KindStep, Title: "Do work", Instructions: "work"}}
	wfRec := call(t, anon(h), http.MethodPost, "/api/workflows", wfBody(t, "Other WF", def), headers)
	if wfRec.Code != http.StatusCreated {
		t.Fatalf("create wf for other = %d %s", wfRec.Code, wfRec.Body.String())
	}
	var wfOut struct{ ID string }
	_ = json.Unmarshal(wfRec.Body.Bytes(), &wfOut)
	runRec := call(t, anon(h), http.MethodPost, "/api/workflow-runs", fmt.Sprintf(`{"workflowId":%q,"input":"Test run"}`, wfOut.ID), headers)
	if runRec.Code != http.StatusCreated {
		t.Fatalf("start run for other = %d %s", runRec.Code, runRec.Body.String())
	}
	var runOut struct{ ID string }
	_ = json.Unmarshal(runRec.Body.Bytes(), &runOut)
	if _, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET status = 'running', step = 0, orchestrator_id = 'orch-other', workspace_id = 'ws-other' WHERE id = $1`, runOut.ID); err != nil {
		t.Fatal(err)
	}
	return headers, keyOut.Key, runOut.ID
}

// assertNoLeak fails if any needle is anywhere a run's state or its agents' messages are kept, in
// the clear: events, commands, the run, the secrets tables (only ciphertext), fetches and extra.
func assertNoLeak(t *testing.T, d dbtest.DB, runID string, extra map[string]string, needles ...string) {
	t.Helper()
	ctx := context.Background()
	checks := map[string]string{
		"workflow_run_events":         `SELECT count(*) FROM workflow_run_events e WHERE e::text LIKE '%' || $1 || '%'`,
		"commands":                    `SELECT count(*) FROM commands c WHERE c::text LIKE '%' || $1 || '%'`,
		"workflow_runs":               `SELECT count(*) FROM workflow_runs r WHERE r::text LIKE '%' || $1 || '%'`,
		"workflow_run_secrets":        `SELECT count(*) FROM workflow_run_secrets s WHERE (s.id, s.name, s.purpose, s.username, s.status, s.stored_by, s.purge_reason)::text LIKE '%' || $1 || '%' OR position(convert_to($1, 'UTF8') IN coalesce(s.ciphertext, '')) > 0`,
		"workflow_run_secret_fetches": `SELECT count(*) FROM workflow_run_secret_fetches f WHERE f::text LIKE '%' || $1 || '%'`,
	}
	for _, needle := range needles {
		for table, q := range checks {
			var n int
			if err := d.Owner.QueryRow(ctx, q, needle).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n > 0 {
				t.Errorf("secret %q leaked into %s", needle, table)
			}
		}
		for where, text := range extra {
			if strings.Contains(text, needle) {
				t.Errorf("secret %q leaked into %s", needle, where)
			}
		}
	}
}

// Not parallel: it captures the server log, which other tests also write to.
func TestWorkflowSecretAskStoreFetchNeverLeaksValue(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	box := testSecretBox(t)
	h, d := dbServerWithSecrets(t, box)
	ctx := context.Background()
	hostKey, runID := setupRunningWorkflow(t, h, d)

	// 1. The orchestrator asks for three secrets: a key, a multi-line PEM and a short PIN.
	code, msg := moveAny(t, h, hostKey, runID, map[string]any{
		"move": "ask",
		"note": "I need the Stripe API key, the deploy key and the PIN to finish setup.",
		"secrets": []map[string]any{
			{"name": "Stripe API key", "purpose": "Read invoices from Stripe"},
			{"name": "Deploy key", "purpose": "Push to the mirror", "username": "deployer", "multiline": true},
			{"name": "PIN"},
		},
	})
	if code != http.StatusOK || !strings.Contains(msg, "Asked Zach") || strings.Contains(msg, "not configured") {
		t.Fatalf("ask move = %d %q", code, msg)
	}

	// 2. The ask shows a masked field for each, the PEM as a text area; no Vaultwarden.
	page := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil)
	html := page.Body.String()
	for _, want := range []string{`type="password"`, `autocomplete="new-password"`, `<textarea class="secret-multiline masked"`, `Stripe API key`,
		`Read invoices from Stripe`, `deployer`, `id="store-secrets"`, `wiped when the run ends or after 7 days`} {
		if !strings.Contains(html, want) {
			t.Errorf("run page missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(html), "vaultwarden") || strings.Contains(html, "not configured") {
		t.Error("run page mentions Vaultwarden or unconfigured storage")
	}

	// 3. The requests are readable through read-only SQL; another account sees none of them.
	sqlCode, sqlRes := sqlPost(t, h, q(`SELECT id::text, name, status, ciphertext IS NULL FROM workflow_run_secrets WHERE run_id = '`+runID+`' ORDER BY position`))
	if sqlCode != http.StatusOK || len(sqlRes.Rows) != 3 || sqlRes.Rows[0][2] != "pending" || sqlRes.Rows[0][3] != true {
		t.Fatalf("SQL secret requests = %d %+v", sqlCode, sqlRes)
	}
	ids := []string{sqlRes.Rows[0][0].(string), sqlRes.Rows[1][0].(string), sqlRes.Rows[2][0].(string)}
	otherHeaders, otherKey, otherRun := otherAccount(t, h, d)
	for _, rel := range []string{"workflow_run_secrets", "workflow_run_secret_fetches"} {
		rec := call(t, anon(h), http.MethodPost, "/api/sql", q(`SELECT * FROM `+rel), otherHeaders)
		var res struct{ Rows [][]any }
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusOK || len(res.Rows) != 0 {
			t.Fatalf("other account reads %s: %d %+v", rel, rec.Code, res)
		}
	}

	secret1 := "sk_live_NEVER_LEAK_WEB_111111111"
	secret2 := "-----BEGIN TEST BLOB-----\nNEVERLEAKPEMLINE2222222\nNEVERLEAKPEMLINE3333333\n-----END TEST BLOB-----\n"
	pin := "4242"
	values := func(text string) string {
		b, _ := json.Marshal(map[string]any{"secrets": []map[string]string{{"id": ids[0], "value": secret1}, {"id": ids[1], "value": secret2}, {"id": ids[2], "value": pin}}, "text": text})
		return string(b)
	}

	// 4. A value pasted into the reply is refused (and not echoed back); nothing is stored.
	bad := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", values("Also pasting "+secret1), sameOriginFetch)
	if bad.Code != http.StatusBadRequest || strings.Contains(bad.Body.String(), secret1) {
		t.Fatalf("value in reply = %d %s", bad.Code, bad.Body.String())
	}
	// Another account cannot store into this run, even with its ids.
	if rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", values(""), otherHeaders); rec.Code != http.StatusNotFound {
		t.Fatalf("other account stores into the run = %d %s", rec.Code, rec.Body.String())
	}
	var stored int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_run_secrets WHERE status <> 'pending'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("refused stores left %d rows stored (%v)", stored, err)
	}

	// 5. Storing all three, with a note holding the PIN's digits as ordinary text (a short value is
	// not mistaken for a paste), seals them and resumes the run in one go.
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", values("Use test mode first; door code 4242 is unrelated."), sameOriginFetch)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /secrets = %d %s (Cache-Control %q)", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if readRun(t, h, runID)["status"] != workflow.Running {
		t.Fatal("run did not resume after storing")
	}

	// 6. The rows hold only ciphertext, bound to their row: it opens there and nowhere else.
	var sealed, sealed2 []byte
	var expires time.Time
	if err := d.Owner.QueryRow(ctx, `SELECT ciphertext, expires_at FROM workflow_run_secrets WHERE id = $1 AND status = 'stored'`, ids[0]).Scan(&sealed, &expires); err != nil {
		t.Fatal(err)
	}
	if err := d.Owner.QueryRow(ctx, `SELECT ciphertext FROM workflow_run_secrets WHERE id = $1`, ids[1]).Scan(&sealed2); err != nil {
		t.Fatal(err)
	}
	var account int64
	if err := d.Owner.QueryRow(ctx, `SELECT account_id FROM workflow_runs WHERE id = $1`, runID).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open(sealed, "intelligence/workflow_run_secrets/v1\x00"+ids[0]+"\x00"+runID+"\x00"+fmt.Sprint(account)); err != nil || string(got) != secret1 {
		t.Fatalf("ciphertext does not open to the value: %v", err)
	}
	if d := time.Until(expires); d < 6*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Errorf("expires in %v, want 7 days", d)
	}
	// Swapping the PEM's ciphertext into the key's row does not give the PEM for the key's name.
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_run_secrets SET ciphertext = $2 WHERE id = $1`, ids[0], sealed2); err != nil {
		t.Fatal(err)
	}
	if code, v, _ := fetchSecretAs(t, h, hostKey, runID, "Stripe API key"); code != http.StatusInternalServerError || v != "" {
		t.Fatalf("swapped ciphertext fetch = %d %q", code, v)
	}
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_run_secrets SET ciphertext = $2 WHERE id = $1`, ids[0], sealed); err != nil {
		t.Fatal(err)
	}

	// 7. The orchestrator is told the names and the fetch commands, never the values.
	var cmdText string
	if err := d.Owner.QueryRow(ctx, `SELECT args->>'text' FROM commands WHERE kind = 'send_message' ORDER BY created_at DESC LIMIT 1`).Scan(&cmdText); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"intelligence secret env --run " + runID + " STRIPE_API_KEY='Stripe API key' -- <command>",
		"intelligence secret get --run " + runID + " --to-file <path> 'Deploy key'",
		"(username: deployer)", "Never print, echo, log", "Use test mode first",
	} {
		if !strings.Contains(cmdText, want) {
			t.Errorf("orchestrator message missing %q:\n%s", want, cmdText)
		}
	}
	if strings.Contains(strings.ToLower(cmdText), "vaultwarden") {
		t.Error("orchestrator message mentions Vaultwarden")
	}
	// An agent's stdout is its transcript: the message offers only env and --to-file, and says the
	// scope is the run's host key, which is what FetchSecret checks.
	for _, bad := range []string{"without --to-file", "to a pipe", "this run's agents"} {
		if strings.Contains(cmdText, bad) {
			t.Errorf("orchestrator message says %q:\n%s", bad, cmdText)
		}
	}
	if !strings.Contains(cmdText, "fetchable only with the host key of host-1") {
		t.Errorf("orchestrator message does not say the secrets are scoped to host-1's key:\n%s", cmdText)
	}

	// 8. The run's host fetches each by name (any case) and each fetch is recorded, not the value.
	for name, want := range map[string]string{"stripe api key": secret1, "Deploy key": secret2, "PIN": pin} {
		if code, v, m := fetchSecretAs(t, h, hostKey, runID, name); code != http.StatusOK || v != want {
			t.Fatalf("fetch %q = %d %q", name, code, m)
		}
	}
	var fetches int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_run_secret_fetches WHERE run_id = $1 AND host = 'host-1'`, runID).Scan(&fetches); err != nil || fetches != 3 {
		t.Fatalf("recorded %d fetches (%v), want 3", fetches, err)
	}
	var fetchEvents int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_run_events WHERE run_id = $1 AND kind = 'secret_fetched'`, runID).Scan(&fetchEvents); err != nil || fetchEvents != 3 {
		t.Fatalf("%d secret_fetched events (%v), want 3", fetchEvents, err)
	}
	// Another account's host cannot fetch this run's secrets, nor can this host fetch another run's.
	if code, v, _ := fetchSecretAs(t, h, otherKey, runID, "Stripe API key"); code != http.StatusNotFound || v != "" {
		t.Fatalf("other account's host fetch = %d %q", code, v)
	}
	if code, _, _ := fetchSecretAs(t, h, hostKey, otherRun, "Stripe API key"); code != http.StatusNotFound {
		t.Fatalf("fetch from another account's run = %d", code)
	}
	// Another host of the same account cannot fetch them either: the key must be the run's host's.
	if code, v, _ := fetchSecretAs(t, h, keyOf(t, h, "host-2"), runID, "Stripe API key"); code != http.StatusNotFound || v != "" {
		t.Fatalf("fetch from another host of the same account = %d %q", code, v)
	}
	if code, _, _ := fetchSecretAs(t, h, "not-a-key", runID, "Stripe API key"); code != http.StatusUnauthorized {
		t.Fatalf("fetch without a host key = %d", code)
	}
	if code, _, m := fetchSecretAs(t, h, hostKey, runID, "Nope"); code != http.StatusBadRequest || !strings.Contains(m, "no secret") {
		t.Fatalf("fetch unknown name = %d %q", code, m)
	}

	// 9. Zach deletes one early from the page: it cannot be fetched again, and another account
	// cannot delete.
	page = call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil)
	if !strings.Contains(page.Body.String(), `data-delete-secret="`+ids[0]+`"`) || !strings.Contains(page.Body.String(), "fetched 1 time") {
		t.Error("run page does not list the stored secret with its fetches and a Delete button")
	}
	if rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+runID+"/secrets/"+ids[0]+"/delete", `{}`, otherHeaders); rec.Code != http.StatusNotFound {
		t.Fatalf("other account deletes = %d", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets/"+ids[0]+"/delete", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if code, _, m := fetchSecretAs(t, h, hostKey, runID, "Stripe API key"); code != http.StatusConflict || !strings.Contains(m, "deleted") {
		t.Fatalf("fetch after delete = %d %q", code, m)
	}

	// 10. Past its expiry a secret cannot be fetched and is wiped.
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_run_secrets SET expires_at = now() - interval '1 second' WHERE id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}
	if code, _, m := fetchSecretAs(t, h, hostKey, runID, "PIN"); code != http.StatusConflict || !strings.Contains(m, "expired") {
		t.Fatalf("fetch after expiry = %d %q", code, m)
	}

	// 11. Everywhere else, the values are nowhere: not in the page, the message, the log, nor any table.
	afterHTML := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil).Body.String()
	assertNoLeak(t, d, runID, map[string]string{"run page HTML": afterHTML, "orchestrator message": cmdText, "server log": logs.String()},
		secret1, secret2, "NEVERLEAKPEMLINE2222222")

	// 12. The run ending wipes what is left.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	var left int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_run_secrets WHERE run_id = $1 AND ciphertext IS NOT NULL`, runID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d ciphertexts left after the run ended (%v)", left, err)
	}
	var reason string
	if err := d.Owner.QueryRow(ctx, `SELECT purge_reason FROM workflow_run_secrets WHERE id = $1`, ids[1]).Scan(&reason); err != nil || reason != "run_ended" {
		t.Fatalf("PEM purge reason = %q (%v)", reason, err)
	}
	if code, _, m := fetchSecretAs(t, h, hostKey, runID, "Deploy key"); code != http.StatusConflict || !strings.Contains(m, "ended") {
		t.Fatalf("fetch after the run ended = %d %q", code, m)
	}
}

func TestWorkflowSecretPurgeExpired(t *testing.T) {
	t.Parallel()
	h, d := dbServerWithSecrets(t, testSecretBox(t))
	hostKey, runID := setupRunningWorkflow(t, h, d)
	moveAny(t, h, hostKey, runID, map[string]any{"move": "ask", "note": "Token?", "secrets": []map[string]string{{"name": "Deploy token"}}})
	id := secretIDs(t, d, runID)[0]
	body, _ := json.Marshal(map[string]any{"secrets": []map[string]string{{"id": id, "value": "tok_EXPIRES_SOON_123"}}})
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(body), sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("store = %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now()
	early := workflow.NewEngine(d.Owner, func() time.Time { return now.Add(time.Hour) })
	if n, err := early.PurgeExpiredSecrets(context.Background()); err != nil || n != 0 {
		t.Fatalf("purged %d an hour in (%v), want 0", n, err)
	}
	late := workflow.NewEngine(d.Owner, func() time.Time { return now.Add(workflow.SecretTTL + time.Minute) })
	if n, err := late.PurgeExpiredSecrets(context.Background()); err != nil || n != 1 {
		t.Fatalf("purged %d past the TTL (%v), want 1", n, err)
	}
	var status, reason string
	var gone bool
	if err := d.Owner.QueryRow(context.Background(), `SELECT status, purge_reason, ciphertext IS NULL FROM workflow_run_secrets WHERE id = $1`, id).Scan(&status, &reason, &gone); err != nil {
		t.Fatal(err)
	}
	if status != "purged" || reason != "expired" || !gone {
		t.Fatalf("after expiry: %s %s wiped=%v", status, reason, gone)
	}
	var ev int
	if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM workflow_run_events WHERE run_id = $1 AND kind = 'secret_expired'`, runID).Scan(&ev); err != nil || ev != 1 {
		t.Fatalf("%d secret_expired events (%v)", ev, err)
	}
}

func TestWorkflowSecretUnconfiguredStoresNothing(t *testing.T) {
	t.Parallel()
	h, d := dbServerWithSecrets(t, nil)
	hostKey, runID := setupRunningWorkflow(t, h, d)
	code, msg := moveAny(t, h, hostKey, runID, map[string]any{"move": "ask", "note": "Need API key",
		"secrets": []map[string]string{{"name": "Demo API key", "purpose": "Testing"}}})
	if code != http.StatusOK || !strings.Contains(msg, "not configured") {
		t.Fatalf("ask move = %d %q", code, msg)
	}
	page := call(t, h, http.MethodGet, "/workflow-runs/"+runID, "", nil).Body.String()
	if !strings.Contains(page, "Secret storage is not configured") || !strings.Contains(page, "INTELLIGENCE_SECRETS_KEY") {
		t.Fatal("run page does not say secret storage is not configured")
	}
	id := secretIDs(t, d, runID)[0]
	val := "never-persist-unconfigured-999"
	body, _ := json.Marshal(map[string]any{"secrets": []map[string]string{{"id": id, "value": val}}})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(body), sameOriginFetch)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), val) {
		t.Fatalf("store unconfigured = %d %s", rec.Code, rec.Body.String())
	}
	if readRun(t, h, runID)["status"] != workflow.Waiting {
		t.Fatal("run should stay waiting when storage is not configured")
	}
	if code, _, _ := fetchSecretAs(t, h, hostKey, runID, "Demo API key"); code != http.StatusServiceUnavailable {
		t.Fatalf("fetch unconfigured = %d", code)
	}
	assertNoLeak(t, d, runID, nil, val)
	// A plain reply still works and dismisses the request.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/reply", `{"text":"Skip this key for now."}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("plain reply = %d %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := d.Owner.QueryRow(context.Background(), `SELECT status FROM workflow_run_secrets WHERE id = $1`, id).Scan(&status); err != nil || status != "dismissed" {
		t.Fatalf("after a plain reply: %q %v", status, err)
	}
}

// Every account can use secrets; each account's are its own.
func TestWorkflowSecretsWorkForEveryAccount(t *testing.T) {
	t.Parallel()
	h, d := dbServerWithSecrets(t, testSecretBox(t))
	headers, otherKey, otherRun := otherAccount(t, h, d)
	if code, msg := moveAny(t, h, otherKey, otherRun, map[string]any{"move": "ask", "note": "Key?",
		"secrets": []map[string]string{{"name": "Other key"}}}); code != http.StatusOK {
		t.Fatalf("other account ask --secret = %d %q", code, msg)
	}
	page := call(t, anon(h), http.MethodGet, "/workflow-runs/"+otherRun, "", map[string]string{"Cookie": headers["Cookie"]}).Body.String()
	if !strings.Contains(page, `id="store-secrets"`) || !strings.Contains(page, `type="password"`) {
		t.Fatal("other account's run page has no secret fields")
	}
	id := secretIDs(t, d, otherRun)[0]
	body, _ := json.Marshal(map[string]any{"secrets": []map[string]string{{"id": id, "value": "other-account-value-1"}}})
	// The first account (signed in by default) cannot store into it.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+otherRun+"/secrets", string(body), sameOriginFetch); rec.Code != http.StatusNotFound {
		t.Fatalf("first account stores into other's run = %d", rec.Code)
	}
	if rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+otherRun+"/secrets", string(body), headers); rec.Code != http.StatusNoContent {
		t.Fatalf("other account stores = %d %s", rec.Code, rec.Body.String())
	}
	if code, v, _ := fetchSecretAs(t, h, otherKey, otherRun, "Other key"); code != http.StatusOK || v != "other-account-value-1" {
		t.Fatalf("other account fetch = %d %q", code, v)
	}
	// The first account's host cannot fetch it.
	sendReport(t, h, wfReport())
	if code, _, _ := fetchSecretAs(t, h, keyOf(t, h, "host-1"), otherRun, "Other key"); code != http.StatusNotFound {
		t.Fatalf("first account's host fetches other's secret = %d", code)
	}
	// Nor read it through SQL.
	_, res := sqlPost(t, h, q(`SELECT id FROM workflow_run_secrets`))
	if len(res.Rows) != 0 {
		t.Fatalf("first account reads other's secrets: %+v", res.Rows)
	}
}

// Storing secrets on a human sign-off step goes through the same reply path as a plain reply:
// "approve" records signoff_approved, and the run's reply event keeps the summary, not the fetch commands.
func TestWorkflowSecretSubmitApprovesSignoff(t *testing.T) {
	t.Parallel()
	h, d := dbServerWithSecrets(t, testSecretBox(t))
	hostKey, runID := setupRunningWorkflowWith(t, h, d, workflow.Step{ID: "signoff", Kind: workflow.KindSignoff,
		Title: "Human sign-off", Instructions: "Ask for approval.", RequireHumanSignoff: true})
	ctx := context.Background()
	if code, msg := moveAny(t, h, hostKey, runID, map[string]any{"move": "ask", "note": "Approve and give me the deploy token.",
		"secrets": []map[string]string{{"name": "Deploy token"}}}); code != http.StatusOK {
		t.Fatalf("ask move = %d %q", code, msg)
	}
	id := secretIDs(t, d, runID)[0]
	body, _ := json.Marshal(map[string]any{"secrets": []map[string]string{{"id": id, "value": "tok_NEVER_LEAK_SIGNOFF"}}, "approve": true})
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runID+"/secrets", string(body), sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /secrets approve = %d %s", rec.Code, rec.Body.String())
	}
	var approved, reply, cmdText string
	if err := d.Owner.QueryRow(ctx, `SELECT text FROM workflow_run_events WHERE run_id = $1 AND kind = 'signoff_approved'`, runID).Scan(&approved); err != nil {
		t.Fatalf("no signoff_approved event: %v", err)
	}
	if err := d.Owner.QueryRow(ctx, `SELECT text FROM workflow_run_events WHERE run_id = $1 AND kind = 'reply'`, runID).Scan(&reply); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, `Stored 1 secret(s) in Intelligence: "Deploy token"`) || strings.Contains(reply, "intelligence secret") || approved != reply {
		t.Errorf("reply event %q, signoff_approved %q: want the stored summary", reply, approved)
	}
	if err := d.Owner.QueryRow(ctx, `SELECT args->>'text' FROM commands WHERE kind = 'send_message' ORDER BY created_at DESC LIMIT 1`).Scan(&cmdText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdText, "explicitly approved") || !strings.Contains(cmdText, "intelligence secret env --run "+runID) || strings.Contains(cmdText, "tok_NEVER_LEAK_SIGNOFF") {
		t.Errorf("orchestrator message = %q", cmdText)
	}
	assertNoLeak(t, d, runID, map[string]string{"orchestrator message": cmdText}, "tok_NEVER_LEAK_SIGNOFF")
}
