package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/hostwatch"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type fakeQuotaNotifier struct {
	notices []hostwatch.Notice
	err     error
}

func (f *fakeQuotaNotifier) Notify(_ context.Context, n hostwatch.Notice) error {
	if f.err != nil {
		return f.err
	}
	f.notices = append(f.notices, n)
	return nil
}

func setupModelQuotaFixture(t *testing.T) (*testServer, dbtest.DB, *fakeClock, *workflow.Engine, *rawHost, string, string) {
	t.Helper()
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)}
	h := newStallServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now })
	e := workflow.NewEngine(d.Owner, clock.Now)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{
		{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Thinking: []string{"high"}}}},
	}
	keyOf(t, h, "host-1")
	b := stallReport(t, h, rep)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('claude','one',now(),now())`)
	exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','claude','one',true,now())`)
	exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
		VALUES('claude','one','7d',604800,100,now()+interval '1 day',now(),now())`)
	exec(`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','claude','one',now(),now())`)
	body, _ := json.Marshal(map[string]any{
		"name": "Claude Opus 5.5",
		"members": []any{map[string]any{
			"provider": "claude", "model": "claude-opus-5-5", "thinking": "high",
			"login": "one", "longWindow": "7d", "enabled": true,
		}},
	})
	rec := call(t, h, "POST", "/api/model-groups", string(body), sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create model group: %d %s", rec.Code, rec.Body)
	}
	var g workflow.ModelGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	def := workflow.Starting
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps[0].Instructions = "Build with {{" + g.Token + "}}. Task: {{input}}"
	def.Steps[1].Instructions = "Check with {{" + g.Token + "}}."
	wfID := createWorkflow(t, h, def)
	return h, d, clock, e, b, wfID, g.Token
}

func TestModelQuotaBlockWaitsAndResumesWithoutAsk(t *testing.T) {
	h, d, clock, e, b, wfID, token := setupModelQuotaFixture(t)
	run := startRun(t, h, wfID, "Fix the widget")
	cmd := b.command()
	var ws hostclient.CreateWorkspaceArgs
	if err := json.Unmarshal(cmd.Args, &ws); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ws.Agent.Prompt, "No eligible model in "+token) {
		t.Fatalf("expected no eligible model message in prompt: %s", ws.Agent.Prompt)
	}
	if strings.Contains(ws.Agent.Prompt, "or ask for an explicit model") {
		t.Fatalf("prompt must not tell orchestrator to ask for an explicit model: %s", ws.Agent.Prompt)
	}
	if !strings.Contains(ws.Agent.Prompt, "model quota") {
		t.Fatalf("orchestrator prompt should explain model quota wait state: %s", ws.Agent.Prompt)
	}
	succeed(t, b, cmd, `{"workspaceId":"w-1","agentId":"orch-1"}`)

	// Run is in running status with activity_waiting_on = 'model quota' and zero asks.
	code, r := sqlPost(t, h, q("SELECT status, activity_waiting_on, current_activity FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != "running" || r.Rows[0][1] != "model quota" || r.Rows[0][2] == "" {
		t.Fatalf("expected running + waiting on model quota, got %d %+v", code, r)
	}
	code, r = sqlPost(t, h, q("SELECT token, resolved_at FROM workflow_model_waits WHERE run_id='"+run+"'"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != token || r.Rows[0][1] != nil {
		t.Fatalf("expected open workflow_model_waits row, got %d %+v", code, r)
	}

	// Polling suggest while quota is still exhausted keeps the wait open without creating an ask.
	key := keyOf(t, h, "host-1")
	clock.Add(2 * time.Minute)
	rec := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/suggestions", `{"token":"`+token+`"}`, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No eligible model") || strings.Contains(rec.Body.String(), "or ask for an explicit model") {
		t.Fatalf("poll suggest: %d %s", rec.Code, rec.Body)
	}

	// Even if an orchestrator tries to ask Zach about the missing model, it is intercepted as a wait state.
	status, msg := move(t, h, key, run, map[string]string{
		"move": "ask",
		"note": "No eligible model in " + token + ". May I launch the builder on claude/claude-opus-5-5 directly?",
	})
	if status != 200 || !strings.Contains(msg, "Not asked") {
		t.Fatalf("expected model-only ask to be intercepted as wait state, got %d %s", status, msg)
	}
	code, r = sqlPost(t, h, q("SELECT status, activity_waiting_on FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || r.Rows[0][0] != "running" || r.Rows[0][1] != "model quota" {
		t.Fatalf("run should stay running on model quota, got %+v", r)
	}
	code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_events WHERE run_id='"+run+"' AND kind='ask'"))
	if code != 200 || r.Rows[0][0] != float64(0) {
		t.Fatalf("expected zero ask events, got %+v", r)
	}

	// Stall watchdog must NOT stall a run that is waiting on model quota.
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{
		{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Thinking: []string{"high"}}}},
	}
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-1", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
	for _, dt := range []time.Duration{0, 5 * time.Minute, 15 * time.Minute} {
		clock.Add(dt)
		if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
			t.Fatal(ack.Error)
		}
		if err := e.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	code, r = sqlPost(t, h, q("SELECT status FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || r.Rows[0][0] != "running" {
		t.Fatalf("stall watchdog must not stall model-quota wait: %+v", r)
	}

	// Quota returns! ReconcileModelWaits resumes the run automatically with a fresh suggestion.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE usage_readings SET used_pct=10, last_observed=now() WHERE provider='claude' AND account='one'`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumeCmd := b.command()
	if resumeCmd.Kind != hostclient.SendMessageKind {
		t.Fatalf("expected send_message command, got %s", resumeCmd.Kind)
	}
	var args hostclient.AgentArgs
	if err := json.Unmarshal(resumeCmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.AgentID != "orch-1" || !strings.Contains(args.Text, "--provider claude/claude-opus-5-5") {
		t.Fatalf("unexpected resume message: %+v", args)
	}
	succeed(t, b, resumeCmd, `{}`)

	code, r = sqlPost(t, h, q("SELECT status, activity_waiting_on FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || r.Rows[0][0] != "running" || r.Rows[0][1] != "" {
		t.Fatalf("expected waiting_on cleared after resume, got %+v", r)
	}
	code, r = sqlPost(t, h, q("SELECT resolved_at IS NOT NULL, resolved_reason FROM workflow_model_waits WHERE run_id='"+run+"'"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != true {
		t.Fatalf("expected resolved model wait, got %+v", r)
	}
}

func TestModelQuotaOutageSendsAtMostOneHostNotice(t *testing.T) {
	h, d, clock, e, b, wfID, token := setupModelQuotaFixture(t)
	notifier := &fakeQuotaNotifier{}
	e.Notify = notifier
	e.NotifyAccount = h.acct.Username
	e.NotifyLocation = time.UTC

	var runs []string
	for i, title := range []string{"First task", "Second task", "Third task"} {
		run := startRun(t, h, wfID, title)
		cmd := b.command()
		succeed(t, b, cmd, `{"workspaceId":"w-`+string(rune('1'+i))+`","agentId":"orch-`+string(rune('1'+i))+`"}`)
		runs = append(runs, run)
	}

	// Below 30 min threshold: no notice yet.
	clock.Add(20 * time.Minute)
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("expected 0 notices before 30m threshold, got %d", len(notifier.notices))
	}

	// Past 30 min threshold: exactly ONE host-level notice listing the blocked runs.
	clock.Add(11 * time.Minute)
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("expected exactly 1 notice after 30m, got %d: %+v", len(notifier.notices), notifier.notices)
	}
	n := notifier.notices[0]
	if !strings.Contains(n.Title, "host-1") || !strings.Contains(n.Body, token) || !strings.Contains(n.Body, "3 runs") {
		t.Fatalf("unexpected notice content: %+v", n)
	}
	for _, id := range runs {
		if !strings.Contains(n.Body, id[:8]) {
			t.Fatalf("notice should list blocked run %s: %+v", id[:8], n)
		}
	}

	// Subsequent ticks (and another blocked run) must NOT send a second notice.
	run4 := startRun(t, h, wfID, "Fourth task")
	succeed(t, b, b.command(), `{"workspaceId":"w-4","agentId":"orch-4"}`)
	_ = run4
	for _, dt := range []time.Duration{5 * time.Minute, 15 * time.Minute} {
		clock.Add(dt)
		if err := e.ReconcileModelWaits(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("expected still at most 1 notice, got %d", len(notifier.notices))
	}

	// Alert state is readable through read-only SQL.
	code, r := sqlPost(t, h, q("SELECT host, blocked_runs, alert_sent_at IS NOT NULL, recovered_at IS NULL FROM model_quota_alerts"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != "host-1" || r.Rows[0][2] != true || r.Rows[0][3] != true {
		t.Fatalf("unexpected model_quota_alerts row: %d %+v", code, r)
	}

	// Quota returns -> runs resume and alert closes without sending another notice.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE usage_readings SET used_pct=15, last_observed=now() WHERE provider='claude' AND account='one'`); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("expected at most 1 notice across outage+recovery, got %d", len(notifier.notices))
	}
	code, r = sqlPost(t, h, q("SELECT recovered_at IS NOT NULL FROM model_quota_alerts"))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != true {
		t.Fatalf("expected alert recovered, got %+v", r)
	}
}

func TestParkedModelOnlyAskResumesAutomatically(t *testing.T) {
	h, d, clock, e, b, wfID, token := setupModelQuotaFixture(t)

	// Run 1: already parked on a model-only ask (simulating runs parked before this change).
	parked := startRun(t, h, wfID, "Check the PR")
	succeed(t, b, b.command(), `{"workspaceId":"w-parked","agentId":"orch-parked"}`)
	// Clear any workflow_model_waits row so this truly tests a pre-existing parked run.
	if _, err := d.Owner.Exec(t.Context(), `DELETE FROM workflow_model_waits WHERE run_id=$1`, parked); err != nil {
		t.Fatal(err)
	}
	askNote := "No eligible model in " + token + ". May I launch the checker on claude/claude-opus-5-5 directly?"
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='waiting', note=$2, activity_waiting_on='Zach' WHERE id=$1`, parked, askNote); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,at,kind,step,text)
		SELECT id,account_id,$2,'ask',step,$3 FROM workflow_runs WHERE id=$1`, parked, clock.Now(), askNote); err != nil {
		t.Fatal(err)
	}

	// Run 2: parked on a genuine human question (must NOT be auto-resumed!).
	human := startRun(t, h, wfID, "Redesign API")
	succeed(t, b, b.command(), `{"workspaceId":"w-human","agentId":"orch-human"}`)
	if _, err := d.Owner.Exec(t.Context(), `DELETE FROM workflow_model_waits WHERE run_id=$1`, human); err != nil {
		t.Fatal(err)
	}
	humanNote := "Should we remove the legacy /v1 endpoint or keep it deprecated for 30 days?"
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='waiting', note=$2, activity_waiting_on='Zach' WHERE id=$1`, human, humanNote); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events(run_id,account_id,at,kind,step,text)
		SELECT id,account_id,$2,'ask',step,$3 FROM workflow_runs WHERE id=$1`, human, clock.Now(), humanNote); err != nil {
		t.Fatal(err)
	}

	// While quota is still exhausted, neither run resumes.
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}
	code, r := sqlPost(t, h, q("SELECT status FROM workflow_runs WHERE id='"+parked+"'"))
	if code != 200 || r.Rows[0][0] != "waiting" {
		t.Fatalf("parked run should still wait while no model is eligible: %+v", r)
	}

	// Quota returns -> parked model-only ask resumes automatically; human ask stays waiting!
	if _, err := d.Owner.Exec(t.Context(), `UPDATE usage_readings SET used_pct=10, last_observed=now() WHERE provider='claude' AND account='one'`); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if err := e.ReconcileModelWaits(t.Context()); err != nil {
		t.Fatal(err)
	}

	cmd := b.command()
	var args hostclient.AgentArgs
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.AgentID != "orch-parked" || !strings.Contains(args.Text, "--provider claude/claude-opus-5-5") || !strings.Contains(args.Text, "resolved") {
		t.Fatalf("unexpected resume message for parked ask: %+v", args)
	}
	succeed(t, b, cmd, `{}`)

	code, r = sqlPost(t, h, q("SELECT status, note, activity_waiting_on FROM workflow_runs WHERE id='"+parked+"'"))
	if code != 200 || r.Rows[0][0] != "running" || r.Rows[0][1] != "" || r.Rows[0][2] != "" {
		t.Fatalf("parked model-only ask should be running with cleared note/waiting_on: %+v", r)
	}
	code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_events WHERE run_id='"+parked+"' AND kind='resume'"))
	if code != 200 || r.Rows[0][0] != float64(1) {
		t.Fatalf("expected resume event for resolved ask: %+v", r)
	}

	// Genuine human ask must remain untouched.
	code, r = sqlPost(t, h, q("SELECT status, note, activity_waiting_on FROM workflow_runs WHERE id='"+human+"'"))
	if code != 200 || r.Rows[0][0] != "waiting" || r.Rows[0][1] != humanNote || r.Rows[0][2] != "Zach" {
		t.Fatalf("genuine human ask was disturbed: %+v", r)
	}
}
