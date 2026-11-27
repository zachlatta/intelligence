package grimm

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestMain(m *testing.M) { os.Exit(dbtest.RunWithTemplate(m)) }

// rig is a test database with an account that holds host-1, online, with Claude Opus 5.5 and
// Codex in its model groups and headroom on both, and a Grimm service whose clock tests can move.
type rig struct {
	t    *testing.T
	d    dbtest.DB
	s    *Service
	reg  *fleet.Registry
	acct int64

	mu     sync.Mutex
	skew   time.Duration
	agents []hostclient.Agent
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := dbtest.New(t)
	r := &rig{t: t, d: d, reg: fleet.New(d.Owner)}
	r.s = &Service{DB: d.Owner, Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return time.Now().Add(r.skew) }}
	ctx := t.Context()
	r.exec(`INSERT INTO accounts (username, password_hash, role_name) VALUES ('zach', 'x', 'zach_role')`)
	if err := d.Owner.QueryRow(ctx, `SELECT id FROM accounts`).Scan(&r.acct); err != nil {
		t.Fatal(err)
	}
	r.exec(`INSERT INTO host_keys (host, account_id, key_hash, created_at) VALUES ('host-1', $1, 'hash', now())`, r.acct)
	r.report()
	r.exec(`UPDATE hosts SET stream_id = gen_random_uuid(), last_seen = now()`)
	for _, p := range []string{"claude", "codex"} {
		r.exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES($1,'zach',now(),now())`, p)
		r.exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1',$1,'zach',true,now())`, p)
		r.exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
			VALUES($1,'zach','7d',604800,20,now()+interval '1 day',now(),now())`, p)
		r.exec(`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1',$1,'zach',now(),now())`, p)
	}
	wf := workflow.NewEngine(d.Owner, time.Now)
	if _, err := wf.SaveModelGroup(ctx, r.acct, "", workflow.ModelGroup{Name: "Frontier", Members: []workflow.ModelMember{
		{Provider: "claude", Model: "claude-opus-5-5", Thinking: "medium", Login: "zach", LongWindow: "7d", Enabled: true},
		{Provider: "codex", Model: "gpt-6.2-sol", Thinking: "high", Login: "zach", LongWindow: "7d", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.d.Owner.Exec(r.t.Context(), q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

func (r *rig) later(d time.Duration) { r.mu.Lock(); r.skew += d; r.mu.Unlock() }

// report sends host-1's report: its providers, the session worktree, and r.agents.
func (r *rig) report() {
	r.t.Helper()
	rep := hostclient.Report{Name: "host-1", Version: "v", Paseo: hostclient.PaseoStatus{Connected: true,
		Providers: []hostclient.Provider{
			{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Thinking: []string{"medium"}}}},
			{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "gpt-6.2-sol", Thinking: []string{"high"}}}},
		},
		Workspaces: []hostclient.Workspace{{ID: "ws-1", Name: "grimm", Kind: "worktree", Status: "running", Directory: "/w/app-grimm"}},
		Agents:     r.agents}}
	if err := r.reg.Record(r.t.Context(), rep, time.Now()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) setAgent(id, status string) {
	r.mu.Lock()
	found := false
	for i := range r.agents {
		if r.agents[i].ID == id {
			r.agents[i].Status, found = status, true
		}
	}
	if !found {
		r.agents = append(r.agents, hostclient.Agent{ID: id, WorkspaceID: "ws-1", Provider: "claude", Status: status, CreatedAt: time.Now()})
	}
	r.mu.Unlock()
	r.report()
}

type cmd struct {
	ID, Kind string
	Args     map[string]any
}

// commands are the host's queued commands, oldest first, since after.
func (r *rig) commands() []cmd {
	r.t.Helper()
	rows, err := r.d.Owner.Query(r.t.Context(), `SELECT id::text, kind, args FROM commands WHERE status = 'queued' ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []cmd
	for rows.Next() {
		var c cmd
		var args []byte
		if err := rows.Scan(&c.ID, &c.Kind, &args); err != nil {
			r.t.Fatal(err)
		}
		_ = json.Unmarshal(args, &c.Args)
		out = append(out, c)
	}
	return out
}

// take is the one queued command of kind, which it marks sent so the next take does not see it.
func (r *rig) take(kind string) cmd {
	r.t.Helper()
	var found []cmd
	for _, c := range r.commands() {
		if c.Kind == kind {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("want one queued %s, have %+v", kind, r.commands())
	}
	r.exec(`UPDATE commands SET status = 'sent' WHERE id = $1`, found[0].ID)
	return found[0]
}

func (r *rig) succeed(c cmd, result string) {
	r.t.Helper()
	r.exec(`UPDATE commands SET status = 'succeeded', result = $2, finished_at = now() WHERE id = $1`, c.ID, result)
}

func (r *rig) reconcile(id string) {
	r.t.Helper()
	if err := r.s.Reconcile(r.t.Context(), id); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) one(q string, args ...any) (v any) {
	r.t.Helper()
	if err := r.d.Owner.QueryRow(r.t.Context(), q, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return v
}

// ready is a session whose worktree is made and whose manager is running as agent "mgr".
func (r *rig) ready() string {
	r.t.Helper()
	ctx := r.t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.succeed(r.take("create_workspace"), `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	if _, err := r.s.Post(ctx, r.acct, id, "Build the CSV tool"); err != nil {
		r.t.Fatal(err)
	}
	r.succeed(r.take("create_agent"), `{"agentId":"mgr"}`)
	r.reconcile(id)
	r.setAgent("mgr", "running")
	return id
}

func TestASessionMakesAWorktreeAndStartsOpusOnTheFirstMessage(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		t.Fatal(err)
	}
	ws := r.take("create_workspace")
	if ws.Args["cwd"] != "/w/app" || ws.Args["worktree"] != true || ws.Args["branch"] != "grimm/"+id[:8] {
		t.Fatalf("workspace command = %+v", ws.Args)
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions`); got != int32(20) {
		t.Errorf("default cap = %v, want 20", got)
	}
	// the first message waits for the worktree
	if _, err := r.s.Post(ctx, r.acct, id, "Build the CSV tool"); err != nil {
		t.Fatal(err)
	}
	if cs := r.commands(); len(cs) != 0 {
		t.Fatalf("started something before the worktree: %+v", cs)
	}
	r.succeed(ws, `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	mgr := r.take("create_agent")
	labels, _ := mgr.Args["labels"].(map[string]any)
	if mgr.Args["provider"] != "claude" || mgr.Args["model"] != "claude-opus-5-5" || mgr.Args["mode"] != "bypassPermissions" ||
		mgr.Args["workspaceId"] != "ws-1" || labels[hostclient.LiveLabel] != hostclient.LiveFast || labels[SessionLabel] != id {
		t.Fatalf("manager = %+v", mgr.Args)
	}
	prompt, _ := mgr.Args["prompt"].(string)
	if !strings.Contains(prompt, "Build the CSV tool") || !strings.Contains(prompt, "at most 20 live workers") || !strings.Contains(prompt, "intelligence grimm ctl hire") {
		t.Errorf("manager prompt lacks the message, the cap or the CLI:\n%s", prompt)
	}
	if got := r.one(`SELECT manager_state FROM grimm_session_status`); got != "starting" {
		t.Errorf("state = %v, want starting", got)
	}
	r.succeed(mgr, `{"agentId":"mgr"}`)
	r.reconcile(id)
	r.setAgent("mgr", "running")
	if got := r.one(`SELECT manager_state FROM grimm_session_status`); got != "working" {
		t.Errorf("state = %v, want working", got)
	}
	// a message mid-turn folds into the running turn
	if _, err := r.s.Post(ctx, r.acct, id, "Also add a --header flag"); err != nil {
		t.Fatal(err)
	}
	steer := r.take("send_message")
	if steer.Args["agentId"] != "mgr" || steer.Args["text"] != "Also add a --header flag" || steer.Args["behavior"] != "steer" {
		t.Fatalf("mid-turn message = %+v", steer.Args)
	}
	// interrupt, then /new ends the manager and the next message starts a fresh one
	if err := r.s.Interrupt(ctx, r.acct, id); err != nil {
		t.Fatal(err)
	}
	if c := r.take("cancel_agent"); c.Args["agentId"] != "mgr" {
		t.Fatalf("interrupt = %+v", c.Args)
	}
	if _, err := r.s.Post(ctx, r.acct, id, "/new"); err != nil {
		t.Fatal(err)
	}
	if c := r.take("archive_agent"); c.Args["agentId"] != "mgr" {
		t.Fatalf("/new archived %+v", c.Args)
	}
	if _, err := r.s.Post(ctx, r.acct, id, "Start over"); err != nil {
		t.Fatal(err)
	}
	if c := r.take("create_agent"); !strings.Contains(c.Args["prompt"].(string), "Start over") {
		t.Fatalf("second manager = %+v", c.Args)
	}
	if got := r.one(`SELECT max(epoch) FROM grimm_managers`); got != int32(2) {
		t.Errorf("epochs = %v", got)
	}
}

func TestASessionNeedsAConnectedHostOfTheAccount(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	for _, n := range []NewSession{{Host: "host-2", Repo: "/w/app"}, {Host: "host-1", Repo: "w/app"}, {Host: "host-1", Repo: "/srv/x/desk"},
		{Host: "host-1", Repo: "/w/app", MaxWorkers: 101}} {
		if _, err := r.s.CreateSession(ctx, r.acct, n); err == nil {
			t.Errorf("made a session with %+v", n)
		}
	}
	r.exec(`UPDATE hosts SET stream_id = NULL`)
	if _, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("offline host: %v", err)
	}
}

func TestHiresKeepToTheCapAndTheRolePolicy(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	cap := 2
	if err := r.s.UpdateSession(ctx, r.acct, id, SessionUpdate{MaxWorkers: &cap}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Role: "reviewer", Provider: "codex", Task: "review"}); err == nil {
		t.Error("a reviewer was hired on Codex")
	}
	rev, err := r.s.HireWorker(ctx, r.acct, id, Hire{Role: "reviewer", Task: "review the change"})
	if err != nil {
		t.Fatal(err)
	}
	if rev.Provider != "claude" || rev.Model != "claude-opus-5-5" || rev.Name != "athena" {
		t.Errorf("reviewer = %+v", rev)
	}
	c := r.take("create_agent")
	if c.Args["title"] != "Grimm/athena (reviewer)" || !strings.Contains(c.Args["prompt"].(string), "intelligence grimm bus post --to grimm --kind result") {
		t.Errorf("worker command = %+v", c.Args)
	}
	// the spread penalty sends the next plain hire to the other plan
	w, err := r.s.HireWorker(ctx, r.acct, id, Hire{Name: "Ash Smith", Task: "build it"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Provider != "codex" || w.Name != "ash-smith" {
		t.Errorf("second hire = %+v, want codex as ash-smith", w)
	}
	if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Task: "one more"}); err == nil || !strings.Contains(err.Error(), "cap reached (2/2)") {
		t.Errorf("over the cap: %v", err)
	}
	if _, err := r.s.ArchiveWorkers(ctx, r.acct, id, "athena"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Task: "one more"}); err != nil {
		t.Errorf("after archiving: %v", err)
	}
}

// started is a hire that the host started as agent.
func (r *rig) started(id, name, agent string) {
	r.t.Helper()
	if _, err := r.s.HireWorker(r.t.Context(), r.acct, id, Hire{Name: name, Task: "do " + name + "'s part"}); err != nil {
		r.t.Fatal(err)
	}
	r.succeed(r.take("create_agent"), `{"agentId":"`+agent+`"}`)
	r.reconcile(id)
	r.setAgent(agent, "running")
	r.reconcile(id)
}

func TestAFinishedWorkerWakesTheIdleManagerOnceWithItsReply(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready()
	r.started(id, "athena", "w1")
	r.started(id, "hermes", "w2")
	if err := r.reg.StoreTranscript(t.Context(), hostclient.TranscriptBatch{Host: "host-1", AgentID: "w1", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{{SeqStart: 1, SeqEnd: 1, Item: json.RawMessage(`{"type":"assistant_message","text":"Done: csv2md works, 12 tests pass."}`)}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.setAgent("w1", "idle")
	r.reconcile(id)
	if got := r.one(`SELECT status || ': ' || last_response FROM grimm_workers WHERE name = 'athena'`); got != "idle: Done: csv2md works, 12 tests pass." {
		t.Fatalf("athena = %v", got)
	}
	// the manager is busy: the news waits
	r.later(5 * time.Second)
	r.reconcile(id)
	if n := len(r.commands()); n != 0 {
		t.Fatalf("woke a busy manager: %+v", r.commands())
	}
	// hermes posts its result to grimm, which is news at once; its end of turn is not news again
	if _, err := r.s.AgentCall(t.Context(), nil, r.acct, "host-1", "w2", Call{Verb: "bus-post", To: "grimm", Kind: "result", Text: "Tests written."}); err != nil {
		t.Fatal(err)
	}
	r.setAgent("w2", "idle")
	r.reconcile(id)
	r.setAgent("mgr", "idle")
	r.reconcile(id) // within the debounce of hermes's post
	if n := len(r.commands()); n != 0 {
		t.Fatalf("woke before the news settled: %+v", r.commands())
	}
	r.later(5 * time.Second)
	r.reconcile(id)
	up := r.take("send_message")
	text, _ := up.Args["text"].(string)
	if up.Args["agentId"] != "mgr" || !strings.HasPrefix(text, "[Grimm update]") || !strings.Contains(text, "csv2md works") ||
		!strings.Contains(text, "Tests written.") || strings.Count(text, "hermes") != 1 {
		t.Fatalf("update = %s", text)
	}
	r.reconcile(id)
	if n := len(r.commands()); n != 0 {
		t.Fatalf("woke twice: %+v", r.commands())
	}
	if got := r.one(`SELECT count(*) FROM grimm_thread WHERE kind = 'update'`); got != int64(1) {
		t.Errorf("thread updates = %v", got)
	}
}

func TestTheBusSpeaksForTheCallingAgentOnly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	r.started(id, "athena", "w1")
	r.started(id, "hermes", "w2")
	out, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "w1", Call{Verb: "bus-post", To: "hermes", Kind: "handoff", Text: "Parser is done; take the CLI."})
	if err != nil || !strings.Contains(out, "delivered") {
		t.Fatalf("post = %q, %v", out, err)
	}
	c := r.take("send_message")
	if c.Args["agentId"] != "w2" || !strings.Contains(c.Args["text"].(string), "handoff from athena") {
		t.Fatalf("delivery = %+v", c.Args)
	}
	if got := r.one(`SELECT sender || '>' || recipient FROM grimm_messages WHERE channel = 'bus' AND kind = 'handoff'`); got != "athena>hermes" {
		t.Errorf("bus row = %v", got)
	}
	for _, bad := range []struct {
		agent string
		call  Call
	}{
		{"w1", Call{Verb: "hire", Text: "more help"}},  // only the manager hires
		{"stranger", Call{Verb: "roster"}},             // not a Grimm agent
		{"", Call{Verb: "roster"}},                     // no PASEO_AGENT_ID
		{"w1", Call{Verb: "say", Args: []string{"x"}}}, // only the manager says
	} {
		if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", bad.agent, bad.call); err == nil {
			t.Errorf("%s %+v was allowed", bad.agent, bad.call)
		}
	}
	// the manager's hire goes through the same cap and balancer
	out, err = r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "hire", Role: "verifier", Text: "write tests from the spec"})
	if err != nil || !strings.HasPrefix(out, "hired ") {
		t.Fatalf("manager hire = %q, %v", out, err)
	}
}

func TestADirectiveReachesEachWorkerOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	r.started(id, "athena", "w1")
	r.started(id, "hermes", "w2")
	r.setAgent("w2", "idle")
	r.reconcile(id)
	seq, pushed, err := r.s.AddDirective(ctx, r.acct, id, "Use tabs.")
	if err != nil || seq != 2 || len(pushed) != 1 || pushed[0] != "athena" {
		t.Fatalf("add = %d %v %v", seq, pushed, err)
	}
	if c := r.take("send_message"); c.Args["agentId"] != "w1" || !strings.Contains(c.Args["text"].(string), "Use tabs.") {
		t.Fatalf("push = %+v", c.Args)
	}
	// the idle one gets it with its next message, and athena does not get it again
	for _, name := range []string{"hermes", "athena"} {
		if _, err := r.s.SayToWorker(ctx, r.acct, id, name, "status?"); err != nil {
			t.Fatal(err)
		}
	}
	cs := r.commands()
	if len(cs) != 2 || !strings.Contains(cs[0].Args["text"].(string), "Use tabs.") || strings.Contains(cs[1].Args["text"].(string), "Use tabs.") {
		t.Fatalf("messages = %+v", cs)
	}
}

func TestThePageCommandsRunWithoutAManagerTurn(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	r.started(id, "athena", "w1")
	for _, tc := range []struct{ text, want string }{
		{"/roster", "athena (worker)"},
		{"/quota", "NEXT HIRE"},
		{"/remember Write tests first", "saved directive #2"},
		{"/directives", "2. Write tests first"},
		{"/cap 7", "worker cap set to 7"},
		{"/bogus", "unknown command"},
		{"@athena how is it going?", "sent to athena"},
	} {
		row, err := r.s.Post(ctx, r.acct, id, tc.text)
		if err != nil {
			t.Fatalf("%s: %v", tc.text, err)
		}
		out := r.one(`SELECT data->>'output' FROM grimm_messages WHERE id = $1`, row)
		if s, _ := out.(string); !strings.Contains(s, tc.want) {
			t.Errorf("%s: output %q, want %q", tc.text, s, tc.want)
		}
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions`); got != int32(7) {
		t.Errorf("cap = %v", got)
	}
	// with an attachment, an @message still goes to the manager
	r.exec(`INSERT INTO attachments (id, account_id, name, media_type, data) VALUES ('00000000-0000-4000-8000-000000000001', $1, 'shot.png', 'image/png', '\x89')`, r.acct)
	if _, err := r.s.Post(ctx, r.acct, id, "@athena look\n\n[attachment:00000000-0000-4000-8000-000000000001]"); err != nil {
		t.Fatal(err)
	}
	var mgrMsg *cmd
	for _, c := range r.commands() {
		if c.Args["agentId"] == "mgr" {
			mgrMsg = &c
		}
	}
	if mgrMsg == nil {
		t.Fatalf("the attachment did not go to the manager: %+v", r.commands())
	}
	if atts, _ := mgrMsg.Args["attachments"].([]any); len(atts) != 1 || !strings.Contains(mgrMsg.Args["text"].(string), ".factory-scratch/attachments/") {
		t.Errorf("manager message = %+v", mgrMsg.Args)
	}
}

func TestEverythingSurvivesARestart(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready()
	r.started(id, "athena", "w1")
	// a new service (a restarted server) carries on from the tables alone
	r.s = &Service{DB: r.d.Owner, Now: r.s.Now}
	r.setAgent("w1", "idle")
	r.setAgent("mgr", "idle")
	r.reconcile(id)
	r.s = &Service{DB: r.d.Owner, Now: r.s.Now} // and again, with the news pending
	r.later(5 * time.Second)
	r.reconcile(id)
	if c := r.take("send_message"); !strings.Contains(c.Args["text"].(string), "athena") {
		t.Fatalf("after a restart: %+v", c.Args)
	}
}

func TestSpawnAndFanoutHireFromThePage(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	row, err := r.s.Post(ctx, r.acct, id, "/spawn iris --role builder --provider codex write the parser")
	if err != nil {
		t.Fatal(err)
	}
	if out := r.one(`SELECT data->>'output' FROM grimm_messages WHERE id = $1`, row); !strings.HasPrefix(out.(string), "hired iris (builder) on codex/gpt-6.2-sol") {
		t.Fatalf("/spawn = %v", out)
	}
	row, err = r.s.Post(ctx, r.acct, id, "/fanout 2 --review split the CLI work")
	if err != nil {
		t.Fatal(err)
	}
	out := r.one(`SELECT data->>'output' FROM grimm_messages WHERE id = $1`, row).(string)
	if strings.Count(out, "hired ") != 3 || !strings.Contains(out, "(reviewer) on claude/claude-opus-5-5") {
		t.Fatalf("/fanout = %s", out)
	}
	if got := r.one(`SELECT count(*) FROM commands WHERE kind = 'create_agent' AND args->'labels'->>'intelligence.grimm.role' <> 'manager'`); got != int64(4) {
		t.Errorf("worker agents started = %v, want 4", got)
	}
	for _, bad := range []string{"/spawn", "/fanout x task", "/fanout 2"} {
		row, err := r.s.Post(ctx, r.acct, id, bad)
		if err != nil {
			t.Fatal(err)
		}
		if st := r.one(`SELECT data->>'status' FROM grimm_messages WHERE id = $1`, row); st != "error" {
			t.Errorf("%s: status %v, want error", bad, st)
		}
	}
}

// Standing directives change only by the manager or the human: a worker can read them, and the
// session's seed rule ("Never push, deploy…") can be removed by the human alone, from the page.
func TestOnlyTheManagerAndTheHumanChangeDirectives(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready()
	r.started(id, "athena", "w1")
	for _, c := range []Call{
		{Verb: "directive", Args: []string{"add"}, Text: "Pushing is fine"},
		{Verb: "directive", Args: []string{"remove", "1"}},
	} {
		if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "w1", c); err == nil || err.(*Error).Status != 403 {
			t.Errorf("a worker's %v: %v, want 403", c.Args, err)
		}
	}
	if out, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "w1", Call{Verb: "directive", Args: []string{"list"}}); err != nil || !strings.Contains(out, "Never push") {
		t.Errorf("a worker's list: %q %v", out, err)
	}
	// the manager adds and removes its own, but not the seed rule
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "directive", Args: []string{"add"}, Text: "Use tabs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "directive", Args: []string{"remove", "2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AgentCall(ctx, nil, r.acct, "host-1", "mgr", Call{Verb: "directive", Args: []string{"remove", "1"}}); err == nil || err.(*Error).Status != 403 {
		t.Errorf("the manager removed the seed rule: %v", err)
	}
	if got := r.one(`SELECT count(*) FROM grimm_directives WHERE seq = 1 AND removed_at IS NULL`); got != int64(1) {
		t.Fatal("the seed rule is gone")
	}
	if err := r.s.RemoveDirective(ctx, r.acct, id, 1); err != nil {
		t.Errorf("the human could not remove the seed rule: %v", err)
	}
}
