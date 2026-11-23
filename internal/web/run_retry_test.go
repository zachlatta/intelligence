package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostwatch"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type noticeRecorder struct {
	mu      sync.Mutex
	notices []hostwatch.Notice
}

func (r *noticeRecorder) Notify(_ context.Context, n hostwatch.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
	return nil
}

func (r *noticeRecorder) snapshot() []hostwatch.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hostwatch.Notice, len(r.notices))
	copy(out, r.notices)
	return out
}

func voiceMemoWorkflowDef() workflow.Definition {
	d := workflow.Starting
	d.Host = "host-1"
	d.Trigger = workflow.TriggerVoiceMemo
	d.VoiceMemo = &workflow.VoiceMemoFilter{}
	d.Project.Mode = workflow.ProjectFixed
	d.Project.Directory = "/w/app"
	return d
}

func TestFailedAnswerExpectedRunNotifiesPhone(t *testing.T) {
	d := dbtest.New(t)
	rec := &noticeRecorder{}
	h := newServer(t, d, d.Owner, "v", func(c *Config) {
		c.HostAlerts = rec
		c.PDWAccount = "someone"
	})

	host := sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")

	// Voice memo run (answer_expected = true) fails when its start attempts fail.
	voiceWF := createWorkflow(t, h, voiceMemoWorkflowDef())
	voiceRun := startRun(t, h, voiceWF, "Voice memo: Check the greenhouse temperature\nRecorded: 2026-11-22T07:05:00Z\n")
	// Exhaust retries via direct start failure: set start_retries = StartRetryLimit so the next command failure fails the run.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET start_retries = $2 WHERE id = $1`, voiceRun, workflow.StartRetryLimit); err != nil {
		t.Fatal(err)
	}
	failCommand(t, host, host.command(), "made workspace wks_123, but its agent: agent.create.request: Antigravity effort is selected through the model ID")

	notices := rec.snapshot()
	if len(notices) != 1 {
		t.Fatalf("notices = %d (%+v), want 1 right after failure", len(notices), notices)
	}
	if !strings.Contains(notices[0].Title, "Voice memo failed") {
		t.Errorf("title = %q, want Voice memo failed", notices[0].Title)
	}
	if !strings.Contains(notices[0].Body, "Antigravity effort is selected through the model ID") {
		t.Errorf("body = %q, want plain error reason", notices[0].Body)
	}
	if notices[0].Level != "time-sensitive" {
		t.Errorf("level = %q, want time-sensitive", notices[0].Level)
	}

	// Readable via read-only SQL and not sent twice.
	code, res := sqlPost(t, h, q(`SELECT run_id::text, notify, alert_sent_at IS NOT NULL, alert_attempts FROM workflow_run_failure_alerts WHERE run_id = `+sqlString(voiceRun)))
	if code != http.StatusOK || len(res.Rows) != 1 || res.Rows[0][1] != true || res.Rows[0][2] != true {
		t.Fatalf("failure alerts SQL = %d %+v", code, res)
	}
	eng := workflow.NewEngine(d.Owner, nil)
	eng.Notify = rec
	eng.NotifyAccount = "someone"
	if err := eng.SendFailureAlerts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("re-sent failure alert: %+v", rec.snapshot())
	}

	// Task run (answer_expected = false) does NOT notify the phone when it fails.
	taskRun := startRun(t, h, fixedWorkflow(t, h), "Routine refactor")
	succeed(t, host, host.command(), `{"workspaceId":"w-task","agentId":"orch-task"}`)
	if code, msg := move(t, h, key, taskRun, map[string]string{"move": "fail", "why": "tests broke"}); code != http.StatusOK {
		t.Fatalf("fail move = %d %s", code, msg)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("task run failure sent a phone notification: %+v", rec.snapshot())
	}
}

func TestWorkflowUpdateAutoRetriesFailedStartRuns(t *testing.T) {
	h, d := dbServer(t)
	host := sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")

	wfID := createWorkflow(t, h, voiceMemoWorkflowDef())
	const memoInput = "Voice memo: Check the greenhouse temperature\nRecorded: 2026-11-22T07:05:00Z\n"

	// 1. Eligible: recent failed-to-start run (no orchestrator, no resolution, no retry).
	eligible := startRun(t, h, wfID, memoInput)
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', error='bad model setting', pending_command=NULL, finished_at=now()-interval '1 hour' WHERE id=$1`, eligible); err != nil {
		t.Fatal(err)
	}

	// 2. Negative: failed more than 24h ago.
	tooOld := startRun(t, h, wfID, "Old voice memo")
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', error='old', pending_command=NULL, finished_at=now()-interval '25 hours' WHERE id=$1`, tooOld); err != nil {
		t.Fatal(err)
	}

	// 3. Negative: orchestrator already started before failing.
	hadOrch := startRun(t, h, wfID, "Mid-run failure")
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', orchestrator_id='orch-prev', error='agent failed', pending_command=NULL, finished_at=now()-interval '10 minutes' WHERE id=$1`, hadOrch); err != nil {
		t.Fatal(err)
	}

	// 4. Negative: already resolved as not_needed.
	resolved := startRun(t, h, wfID, "Not a request")
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', error='bad setting', pending_command=NULL, finished_at=now()-interval '10 minutes' WHERE id=$1`, resolved); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_answer_resolutions (run_id, account_id, kind, reason) SELECT id, account_id, 'not_needed', 'handled' FROM workflow_runs WHERE id=$1`, resolved); err != nil {
		t.Fatal(err)
	}

	// Save a new version of the workflow (v2).
	updated := voiceMemoWorkflowDef()
	updated.Orchestrator.Thinking = ""
	body, _ := json.Marshal(map[string]any{"name": "Voice Memo Received", "description": "fixed", "definition": updated})
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfID, string(body), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("update workflow = %d %s", rec.Code, rec.Body)
	}

	// Exactly one retry run should exist, for eligible, on v2 with the same input.
	code, res := sqlPost(t, h, q(`SELECT id::text, version, input, retried_from_run_id::text FROM workflow_runs WHERE retried_from_run_id IS NOT NULL ORDER BY created_at`))
	if code != http.StatusOK || len(res.Rows) != 1 {
		t.Fatalf("retry runs = %d %+v, want 1", code, res)
	}
	retryID := res.Rows[0][0].(string)
	if res.Rows[0][1] != float64(2) || res.Rows[0][2] != memoInput || res.Rows[0][3] != eligible {
		t.Fatalf("retry run = %+v, want v2, same input, from %s", res.Rows[0], eligible)
	}
	// The retry run queued its start command on the host.
	succeed(t, host, host.command(), `{"workspaceId":"w-retry","agentId":"orch-retry"}`)

	// Recording a checked answer on the retry run settles the original failed run too.
	ansBody, _ := json.Marshal(workflow.Answer{Channel: "email", MessageID: "msg-retry-1", SentAt: time.Now().UTC().Add(time.Second), Checked: true})
	if rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+retryID+"/answers", string(ansBody), map[string]string{"Authorization": "Bearer " + key}); rec.Code != http.StatusOK {
		t.Fatalf("record answer = %d %s", rec.Code, rec.Body)
	}
	code, st := sqlPost(t, h, q(`SELECT answered_at IS NOT NULL, answered_via::text, resolution FROM workflow_run_answer_status WHERE id = `+sqlString(eligible)))
	if code != http.StatusOK || len(st.Rows) != 1 || st.Rows[0][0] != true || st.Rows[0][1] != retryID || st.Rows[0][2] != "steered" {
		t.Fatalf("original run answer status = %d %+v", code, st)
	}

	// Manual retry after an automatic re-run must be refused with 409 naming the existing retry,
	// and the Retry button must be hidden on the already-retried run's page.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+eligible+"/retry", `{}`, sameOriginFetch); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), retryID) {
		t.Fatalf("manual retry after auto-retry = %d %q, want 409 mentioning %s", rec.Code, rec.Body.String(), retryID)
	}
	if page := get(t, h, "/workflow-runs/"+eligible, nil).Body.String(); strings.Contains(page, `id="retry"`) {
		t.Fatal("already auto-retried run page should hide Retry button")
	}

	// Updating the workflow again to v3 does NOT re-retry eligible.
	body3, _ := json.Marshal(map[string]any{"name": "Voice Memo Received", "description": "v3", "definition": updated})
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wfID, string(body3), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("update workflow v3 = %d %s", rec.Code, rec.Body)
	}
	code, res = sqlPost(t, h, q(`SELECT count(*) FROM workflow_runs WHERE retried_from_run_id IS NOT NULL`))
	if code != http.StatusOK || res.Rows[0][0] != float64(1) {
		t.Fatalf("retry count after v3 = %d %+v, want 1", code, res)
	}
}

func TestWorkflowRunRetryAPIAndPage(t *testing.T) {
	h, d := dbServer(t)
	host := sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	wfID := createWorkflow(t, h, voiceMemoWorkflowDef())
	const input = "Voice memo: Check the garage door\n"
	runID := startRun(t, h, wfID, input)
	path := "/api/workflow-runs/" + runID + "/retry"

	// Cannot retry while starting/running.
	if rec := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch); rec.Code != http.StatusConflict {
		t.Fatalf("retry while starting = %d %s, want 409", rec.Code, rec.Body)
	}
	succeed(t, host, host.command(), `{"workspaceId":"w-1","agentId":"orch-1"}`)
	if rec := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch); rec.Code != http.StatusConflict {
		t.Fatalf("retry while running = %d %s, want 409", rec.Code, rec.Body)
	}
	if page := get(t, h, "/workflow-runs/"+runID, nil).Body.String(); strings.Contains(page, `id="retry"`) {
		t.Fatal("running run page should not show Retry button")
	}

	// Fail the run.
	if code, msg := move(t, h, key, runID, map[string]string{"move": "fail", "why": "something broke"}); code != http.StatusOK {
		t.Fatalf("fail = %d %s", code, msg)
	}
	if page := get(t, h, "/workflow-runs/"+runID, nil).Body.String(); !strings.Contains(page, `id="retry"`) {
		t.Fatal("failed run page should show Retry button")
	}

	// Auth and origin guards.
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob-retry", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch, sessionFor(t, ts.store, bob)); rec.Code != http.StatusNotFound {
		t.Fatalf("other account retry = %d, want 404", rec.Code)
	}
	if rec := call(t, anon(h), http.MethodPost, path, `{}`, map[string]string{"Authorization": "Bearer " + key}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("host key retry = %d, want 401", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, path, `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site retry = %d, want 403", rec.Code)
	}

	// One-click retry succeeds and returns 201 with new run ID.
	rec := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s, want 201", rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" || out.ID == runID {
		t.Fatalf("retry body = %s, err = %v", rec.Body, err)
	}
	if got := rec.Header().Get("Location"); got != "/workflow-runs/"+out.ID {
		t.Fatalf("Location = %q, want /workflow-runs/%s", got, out.ID)
	}
	code, res := sqlPost(t, h, q(`SELECT input, retried_from_run_id::text FROM workflow_runs WHERE id = `+sqlString(out.ID)))
	if code != http.StatusOK || len(res.Rows) != 1 || res.Rows[0][0] != input || res.Rows[0][1] != runID {
		t.Fatalf("retry run SQL = %d %+v, want input=%q retried_from=%s", code, res, input, runID)
	}
	// Both pages link to each other, and the already-retried run hides the Retry button.
	if page := get(t, h, "/workflow-runs/"+runID, nil).Body.String(); !strings.Contains(page, "/workflow-runs/"+out.ID) || strings.Contains(page, `id="retry"`) {
		t.Fatalf("old run page should link to retry run %s and hide Retry button", out.ID)
	}
	if page := get(t, h, "/workflow-runs/"+out.ID, nil).Body.String(); !strings.Contains(page, "/workflow-runs/"+runID) {
		t.Fatalf("new run page should link to failed run %s", runID)
	}

	// A second POST /retry on the same failed run returns 409 mentioning the existing retry's id.
	if rec := call(t, h, http.MethodPost, path, `{}`, sameOriginFetch); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), out.ID) {
		t.Fatalf("second retry = %d %q, want 409 mentioning %s", rec.Code, rec.Body.String(), out.ID)
	}

	// Deleting the workflow disables retry for an unretried failed run too.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', error='boom', pending_command=NULL, finished_at=now() WHERE id=$1`, out.ID); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wfID, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("delete workflow = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+out.ID+"/retry", `{}`, sameOriginFetch); rec.Code != http.StatusConflict {
		t.Fatalf("retry on deleted workflow = %d %s, want 409", rec.Code, rec.Body)
	}
	if page := get(t, h, "/workflow-runs/"+out.ID, nil).Body.String(); strings.Contains(page, `id="retry"`) {
		t.Fatal("failed run page for deleted workflow should not show Retry button")
	}
}

func TestDatabaseRejectsDuplicateRetryForSameRun(t *testing.T) {
	h, d := dbServer(t)
	host := sendReport(t, h, wfReport())
	wfID := createWorkflow(t, h, voiceMemoWorkflowDef())

	failed := startRun(t, h, wfID, "Voice memo: First attempt\n")
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='failed', error='boom', pending_command=NULL, finished_at=now() WHERE id=$1`, failed); err != nil {
		t.Fatal(err)
	}

	firstRetry := startRun(t, h, wfID, "Voice memo: First attempt\n")
	_ = host.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET retried_from_run_id=$2 WHERE id=$1`, firstRetry, failed); err != nil {
		t.Fatalf("first retry link: %v", err)
	}

	secondRetry := startRun(t, h, wfID, "Voice memo: First attempt\n")
	_ = host.command()
	_, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET retried_from_run_id=$2 WHERE id=$1`, secondRetry, failed)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate retried_from_run_id err = %v, want Postgres 23505 unique_violation", err)
	}
}
