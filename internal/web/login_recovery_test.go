package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/usage"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// What Claude Code said to every agent on the host when its login reached the 30-day limit.
const loginExpiredText = "Failed to authenticate: this login reached its time limit and cannot be renewed. Run `claude auth login` to sign in again, or use `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN for unattended jobs"

type loginRig struct {
	*stallFixture
	b      *rawHost
	engine *workflow.Engine
	run    string
	rep    hostclient.Report
}

// newLoginRig starts a run whose orchestrator (orch-1) has a worker (child), on a host whose
// Claude login is ada@example.com.
func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	f := newStallFixture(t)
	b := f.reset(t)
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, f.h, createWorkflow(t, f.h, def), "recover from a dead login")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{
		{ID: "orch-1", WorkspaceID: "w-run", Title: "Orchestrator", Provider: "claude", Model: "claude-opus-5-5", Status: "idle", RequiresAttention: true, AttentionReason: "finished"},
		{ID: "child", WorkspaceID: "w-run", ParentID: "orch-1", Title: "Builder", Provider: "claude", Model: "claude-opus-5-5", Status: "idle", RequiresAttention: true, AttentionReason: "finished"},
	}
	g := &loginRig{stallFixture: f, b: b, engine: workflow.NewEngine(f.d.Owner, f.clock.Now), run: run, rep: rep}
	g.report(t)
	g.usage(t, "ada@example.com")
	return g
}

func (g *loginRig) report(t *testing.T) {
	t.Helper()
	if ack := g.b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &g.rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

// usage sends a meter read in which active is the login the host's Claude uses.
func (g *loginRig) usage(t *testing.T, active string, broken ...string) {
	t.Helper()
	g.clock.Add(10 * time.Second) // each read is a new one
	now := g.clock.Now()
	var accts []usage.Account
	for _, login := range []string{"ada@example.com", "grace@example.com"} {
		a := usage.Account{ID: login, Label: login, Active: login == active, ObservedAt: now,
			Windows: []usage.Window{{ID: "7d", Seconds: usage.Week, UsedPct: 10, ResetsAt: now.Add(72 * time.Hour)}}}
		for _, b := range broken {
			if b == login {
				a.Windows, a.Error = nil, "cswap usage status: relogin_required"
			}
		}
		accts = append(accts, a)
	}
	if ack := g.b.call(hostclient.Frame{Type: hostclient.UsageFrame, Usage: &usage.Report{CollectedAt: now,
		Sources: []usage.Source{{Provider: usage.Claude, Accounts: accts}}}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

// fail sends agent's transcript ending at seq in the provider's login failure.
func (g *loginRig) fail(t *testing.T, agent string, seq int) {
	t.Helper()
	at := g.clock.Now()
	item, _ := json.Marshal(map[string]string{"type": "assistant_message", "text": loginExpiredText})
	user, _ := json.Marshal(map[string]string{"type": "user_message", "text": "carry on"})
	if ack := g.b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &hostclient.TranscriptBatch{Host: "host-1", AgentID: agent,
		Epoch: "e", Replace: true, Entries: []hostclient.TranscriptEntry{
			{SeqStart: seq - 1, SeqEnd: seq - 1, Provider: "claude", At: at.Add(-time.Second), Item: user},
			{SeqStart: seq, SeqEnd: seq, Provider: "claude", At: at, Item: item}}}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

func (g *loginRig) recover(t *testing.T) {
	t.Helper()
	g.report(t) // the host stays fresh
	if err := g.engine.RecoverInterruptions(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (g *loginRig) sql(t *testing.T, query string) [][]any {
	t.Helper()
	code, r := sqlPost(t, g.h, q(query))
	if code != http.StatusOK {
		t.Fatalf("%s: %d %+v", query, code, r)
	}
	return r.Rows
}

func (g *loginRig) messages(t *testing.T) []hostclient.AgentArgs {
	t.Helper()
	var out []hostclient.AgentArgs
	for _, row := range g.sql(t, "SELECT args::text FROM commands WHERE kind = 'send_message' ORDER BY created_at, id") {
		var a hostclient.AgentArgs
		if err := json.Unmarshal([]byte(row[0].(string)), &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func (g *loginRig) status(t *testing.T, agent string) string {
	t.Helper()
	rows := g.sql(t, "SELECT status FROM agent_interruptions WHERE agent_id = '"+agent+"' ORDER BY seq DESC LIMIT 1")
	if len(rows) == 0 {
		return ""
	}
	return rows[0][0].(string)
}

// The 2026-11-19 case end to end: the login dies under the orchestrator, the stall watchdog must
// not spend its continuations on a login that cannot work, and once the host has a working login
// the same orchestrator (same model, its context intact) is told to carry on, exactly once.
func TestAnOrchestratorStoppedByADeadLoginIsResumedOnceALoginWorks(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "orch-1", 92)
	if s := g.status(t, "orch-1"); s != "waiting" {
		t.Fatalf("status %q", s)
	}
	// the login is still the dead one: nothing to resume yet, and no stall nudges either
	g.recover(t)
	for range 3 {
		g.clock.Add(3 * time.Minute)
		g.recover(t)
		if err := g.engine.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if m := g.messages(t); len(m) != 0 {
		t.Fatalf("messages while the login is dead: %+v", m)
	}
	if n := g.sql(t, "SELECT count(*) FROM workflow_run_continuations")[0][0]; n != float64(0) {
		t.Fatalf("continuations: %v", n)
	}
	// cswap fails over to another login
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	g.recover(t) // and a second pass, or a second server, sends nothing more
	m := g.messages(t)
	if len(m) != 1 || m[0].AgentID != "orch-1" || m[0].Behavior != hostclient.Queue ||
		!strings.Contains(m[0].Text, "did not run") || !strings.Contains(m[0].Text, "ada@example.com") ||
		!strings.Contains(m[0].Text, "Before repeating") {
		t.Fatalf("messages: %+v", m)
	}
	if s := g.status(t, "orch-1"); s != "resumed" {
		t.Fatalf("status %q", s)
	}
	// the run says what happened, and the broken login stays broken until it works again
	events := g.sql(t, "SELECT kind, text FROM workflow_run_events WHERE run_id = '"+g.run+"' AND kind IN ('interrupted', 'login_recovery') ORDER BY id")
	if len(events) != 2 || events[0][0] != "interrupted" || events[1][0] != "login_recovery" {
		t.Fatalf("events: %v", events)
	}
	page := get(t, g.h, "/workflow-runs/"+g.run, nil).Body.String()
	if !strings.Contains(page, "Interrupted: Orchestrator could not run its turn") || !strings.Contains(page, "Resumed") {
		t.Fatal("run page does not show the interruption and its recovery")
	}
	if rows := g.sql(t, "SELECT kind, login, interrupted, recovered FROM login_alerts"); len(rows) != 1 || rows[0][1] != "ada@example.com" || rows[0][3] != float64(1) {
		t.Fatalf("alerts: %v", rows)
	}
	// every page loads the warning, and the Plans page has room for the history
	if plans := get(t, g.h, "/usage", nil).Body.String(); !strings.Contains(plans, `src="/logins.js"`) || !strings.Contains(plans, `id="login-history"`) {
		t.Fatal("the Plans page does not load the login warning and history")
	}
	if js := get(t, g.h, "/logins.js", nil); js.Code != http.StatusOK || !strings.Contains(js.Body.String(), "login_alerts") {
		t.Fatalf("logins.js: %d", js.Code)
	}
	// a new launch is not suggested onto the broken login
	if rows := g.sql(t, "SELECT count(*) FROM login_incidents WHERE resolved_at IS NULL AND login = 'ada@example.com'"); rows[0][0] != float64(1) {
		t.Fatal(rows)
	}
	// someone signs ada in again: its next reading works, and the warning goes
	g.clock.Add(time.Minute)
	g.usage(t, "grace@example.com")
	if rows := g.sql(t, "SELECT count(*) FROM login_alerts"); rows[0][0] != float64(0) {
		t.Fatalf("alerts after the fix: %v", rows)
	}
}

// A worker belongs to its orchestrator, which may already have replaced it: Intelligence tells the
// orchestrator (another agent) to resume or replace it with the same model, rather than starting it
// behind its back.
func TestAWorkerStoppedByADeadLoginIsHandedToItsOrchestrator(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "child", 40)
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	m := g.messages(t)
	if len(m) != 1 || m[0].AgentID != "orch-1" || !strings.Contains(m[0].Text, "paseo send child") ||
		!strings.Contains(m[0].Text, "--provider claude/claude-opus-5-5") || !strings.Contains(m[0].Text, "Builder") {
		t.Fatalf("messages: %+v", m)
	}
	if s := g.status(t, "child"); s != "parent_notified" {
		t.Fatalf("status %q", s)
	}
}

// Both broke: the orchestrator is resumed first, and told about its worker after.
func TestAParentStoppedTooIsResumedBeforeItHearsAboutItsWorker(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "orch-1", 92)
	g.fail(t, "child", 40)
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	g.clock.Add(time.Second)
	g.recover(t)
	m := g.messages(t)
	if len(m) != 2 || m[0].AgentID != "orch-1" || !strings.Contains(m[0].Text, "did not run") ||
		m[1].AgentID != "orch-1" || !strings.Contains(m[1].Text, "paseo send child") {
		t.Fatalf("messages: %+v", m)
	}
}

func TestRecoveryLeavesAloneAnAgentThatAlreadyCarriedOnOrIsGone(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "orch-1", 92)
	g.fail(t, "child", 40)
	// orch-1 was nudged and got going before Intelligence could act
	item, _ := json.Marshal(map[string]string{"type": "assistant_message", "text": "Back at it."})
	if ack := g.b.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &hostclient.TranscriptBatch{Host: "host-1", AgentID: "orch-1",
		Epoch: "e", Entries: []hostclient.TranscriptEntry{{SeqStart: 93, SeqEnd: 93, Provider: "claude", At: g.clock.Now(), Item: item}}}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// and the worker was archived
	g.rep.Paseo.Agents = g.rep.Paseo.Agents[:1]
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	if m := g.messages(t); len(m) != 0 {
		t.Fatalf("messages: %+v", m)
	}
	if a, b := g.status(t, "orch-1"), g.status(t, "child"); a != "superseded" || b != "gone" {
		t.Fatalf("statuses %q %q", a, b)
	}
}

// Recovery is bounded: an agent that keeps failing is left for someone to look at.
func TestRecoveryGivesUpOnAnAgentThatKeepsFailing(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.usage(t, "grace@example.com", "ada@example.com")
	for i := range 4 {
		g.clock.Add(time.Minute)
		g.fail(t, "orch-1", 10+2*i) // on grace, the login in use: that one looks broken now too
		g.clock.Add(time.Minute)
		g.usage(t, "grace@example.com", "ada@example.com") // until its next reading works
		g.recover(t)
	}
	if m := g.messages(t); len(m) != 3 {
		t.Fatalf("%d messages", len(m))
	}
	if s := g.status(t, "orch-1"); s != "gave_up" {
		t.Fatalf("status %q", s)
	}
	// the run says so; its stall watchdog takes it from here (and stalls it if nothing works)
	if rows := g.sql(t, "SELECT text FROM workflow_run_events WHERE kind = 'login_recovery' ORDER BY id DESC LIMIT 1"); len(rows) == 0 ||
		!strings.Contains(rows[0][0].(string), "stopped") {
		t.Fatalf("events: %v", rows)
	}
}

// With no meter on the host, Intelligence cannot tell when the login works again: the warning asks
// Zach to fix it, and marking it fixed (a JSON write) lets the waiting agents carry on.
func TestMarkingALoginFixedResumesWhatWaitedForIt(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t)
	b := f.reset(t)
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, f.h, createWorkflow(t, f.h, def), "recover from a dead login")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	rep := wfReport()
	rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Title: "Orchestrator", Provider: "claude", Model: "claude-opus-5-5", Status: "idle"}}
	g := &loginRig{stallFixture: f, b: b, engine: workflow.NewEngine(f.d.Owner, f.clock.Now), run: run, rep: rep}
	g.report(t)
	g.fail(t, "orch-1", 5)
	g.recover(t)
	if m := g.messages(t); len(m) != 0 {
		t.Fatalf("messages: %+v", m)
	}
	rows := g.sql(t, "SELECT incident_id, login, command, first_step FROM login_alerts")
	if len(rows) != 1 || rows[0][1] != "" || rows[0][2] != "claude auth login --claudeai" || rows[0][3] != true {
		t.Fatalf("alerts: %v", rows)
	}
	path := fmt.Sprintf("/api/login-incidents/%v/resolve", rows[0][0])
	bob, err := g.h.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if r := call(t, g.h, http.MethodPost, path, `{}`, sameOriginFetch, sessionFor(t, g.h.store, bob)); r.Code != 404 {
		t.Fatalf("other account: %d", r.Code)
	}
	if r := call(t, g.h, http.MethodPost, path, `{}`, map[string]string{"Origin": "https://other.example"}); r.Code != 403 {
		t.Fatalf("cross origin: %d", r.Code)
	}
	if r := call(t, anon(g.h), http.MethodPost, path, `{}`, sameOriginFetch); r.Code != 401 {
		t.Fatalf("anonymous: %d", r.Code)
	}
	if r := call(t, g.h, http.MethodPost, path, `{"note": 3}`, sameOriginFetch); r.Code != 400 {
		t.Fatalf("bad body: %d", r.Code)
	}
	if r := call(t, g.h, http.MethodPost, "/api/login-incidents/x/resolve", `{}`, sameOriginFetch); r.Code != 404 {
		t.Fatalf("bad id: %d", r.Code)
	}
	if r := call(t, g.h, http.MethodPost, path, `{"note": "signed in again"}`, sameOriginFetch); r.Code != 204 {
		t.Fatalf("resolve: %d %s", r.Code, r.Body)
	}
	if r := g.sql(t, "SELECT resolved_by, resolved_note FROM login_incidents"); r[0][0] != "manual" || r[0][1] != "signed in again" {
		t.Fatal(r)
	}
	g.recover(t)
	if m := g.messages(t); len(m) != 1 || m[0].AgentID != "orch-1" {
		t.Fatalf("messages: %+v", m)
	}
}

// The 2026-11-19 orchestrator failed twice on the dead login (its turn, then a nudge): it gets one
// resume, for its newest failure, and the older one is settled as superseded.
func TestAnAgentThatFailedTwiceIsResumedOnce(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "orch-1", 92)
	g.clock.Add(2 * time.Minute)
	g.fail(t, "orch-1", 94)
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	g.clock.Add(time.Second)
	g.recover(t)
	if m := g.messages(t); len(m) != 1 || m[0].AgentID != "orch-1" {
		t.Fatalf("messages: %+v", m)
	}
	rows := g.sql(t, "SELECT seq, status FROM agent_interruptions WHERE agent_id = 'orch-1' ORDER BY seq")
	if len(rows) != 2 || rows[0][1] != "superseded" || rows[1][1] != "resumed" {
		t.Fatalf("interruptions: %v", rows)
	}
}

// A message already on its way to the agent (Zach's, say) gets it going: no second one.
func TestAMessageAlreadyOnItsWayIsNotDuplicated(t *testing.T) {
	t.Parallel()
	g := newLoginRig(t)
	g.fail(t, "orch-1", 92)
	if r := call(t, g.h, http.MethodPost, "/api/hosts/host-1/commands", `{"kind":"send_message","args":{"agentId":"orch-1","text":"carry on"}}`, sameOriginFetch); r.Code >= 300 {
		t.Fatalf("queue a message: %d %s", r.Code, r.Body)
	}
	g.usage(t, "grace@example.com", "ada@example.com")
	g.recover(t)
	if m := g.messages(t); len(m) != 1 || m[0].Text != "carry on" {
		t.Fatalf("messages: %+v", m)
	}
	if s := g.status(t, "orch-1"); s != "waiting" {
		t.Fatalf("status %q", s)
	}
}
