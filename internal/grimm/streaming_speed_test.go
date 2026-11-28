package grimm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestManagerDefaultsToOpus55AndSupportsPerSessionModelSelection(t *testing.T) {
	t.Parallel()
	if ManagerProvider != "claude" || ManagerModel != "claude-opus-5-5" {
		t.Fatalf("default manager = %s/%s, want claude/claude-opus-5-5", ManagerProvider, ManagerModel)
	}
	p := PolicyFor("manager")
	if p.Preferred != "claude" || p.Pinned["claude"] != "claude-opus-5-5" {
		t.Fatalf("manager policy = %+v, want claude pinned to claude-opus-5-5", p)
	}

	r := newRig(t)
	r.withNeon()
	ctx := t.Context()

	// Disabled provider refused at session creation.
	if _, err := r.s.CreateSession(ctx, r.acct, NewSession{
		Host: "host-1", Repo: "/w/app", ManagerProvider: "gemini", ManagerModel: "gemini-3-pro",
	}); err == nil {
		t.Fatal("expected disabled provider gemini to be refused")
	}

	// Custom manager model (Antigravity Neon Medium) selected at session creation.
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{
		Host:            "host-1",
		Repo:            "/w/app",
		ManagerProvider: "antigravity",
		ManagerModel:    "gemini-4-neon-medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT manager_provider || '/' || manager_model FROM grimm_session_status WHERE id = $1`, id); got != "antigravity/gemini-4-neon-medium" {
		t.Fatalf("session status before start = %v, want antigravity/gemini-4-neon-medium", got)
	}
	r.succeed(r.take("create_workspace"), `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	mgr := r.take("create_agent")
	if mgr.Args["provider"] != "antigravity" || mgr.Args["model"] != "gemini-4-neon-medium" {
		t.Fatalf("custom manager started on %v/%v, want antigravity/gemini-4-neon-medium", mgr.Args["provider"], mgr.Args["model"])
	}
	if th, _ := mgr.Args["thinking"].(string); th != "" {
		t.Errorf("antigravity manager thinking = %q, want empty", th)
	}
}

func TestManagerPrestartsOnWorktreeReadyAndShowsWorkingImmediately(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		t.Fatal(err)
	}
	ws := r.take("create_workspace")
	r.succeed(ws, `{"workspaceId":"ws-1"}`)
	r.reconcile(id)

	// Manager pre-started as soon as worktree became ready, defaulting to Claude Opus 5.5.
	mgr := r.take("create_agent")
	if mgr.Args["provider"] != "claude" || mgr.Args["model"] != "claude-opus-5-5" {
		t.Fatalf("pre-started manager = %v/%v, want claude/claude-opus-5-5", mgr.Args["provider"], mgr.Args["model"])
	}
	// M2: While create_agent is queued/sent/running, manager_state is 'starting' ("Warming up Grimm…"), not 'working'.
	if got := r.one(`SELECT manager_state FROM grimm_session_status WHERE id = $1`, id); got != "starting" {
		t.Fatalf("manager_state during create_agent = %v, want starting", got)
	}
	// Pre-start should not add a noisy thread bubble.
	if got := r.one(`SELECT count(*) FROM grimm_messages WHERE session_id = $1 AND channel = 'thread'`, id); got != int64(0) {
		t.Fatalf("pre-start created %v thread messages, want 0", got)
	}

	r.succeed(mgr, `{"agentId":"mgr-1"}`)
	r.reconcile(id)
	// M2: While the pre-started manager is running its warm-up turn before any human message,
	// manager_state stays 'starting' and warm-up transcript entries are hidden from grimm_thread.
	r.setAgent("mgr-1", "running")
	if got := r.one(`SELECT manager_state FROM grimm_session_status WHERE id = $1`, id); got != "starting" {
		t.Fatalf("manager_state during pre-start warm-up = %v, want starting", got)
	}
	now := time.Now().UTC()
	if err := r.reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"user_message","text":"warm-up prompt"}`)},
			{SeqStart: 2, SeqEnd: 2, At: now.Add(time.Second), Item: json.RawMessage(`{"type":"tool_call","name":"Bash","status":"completed","detail":{"type":"shell","command":"pdw call skills__skill_read"}}`)},
			{SeqStart: 3, SeqEnd: 3, At: now.Add(2 * time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"Ready."}`)},
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT count(*) FROM grimm_thread WHERE session_id = $1`, id); got != int64(0) {
		t.Fatalf("warm-up entries leaked into grimm_thread: %v rows, want 0", got)
	}

	r.setAgent("mgr-1", "idle")
	if got := r.one(`SELECT manager_state FROM grimm_session_status WHERE id = $1`, id); got != "idle" {
		t.Fatalf("manager_state when idle = %v, want idle", got)
	}

	// Sending a human message queues send_message -> manager_state immediately reports 'working'
	// even before the host reports the agent status as 'running'.
	if _, err := r.s.Post(ctx, r.acct, id, "Hello Grimm"); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT manager_state FROM grimm_session_status WHERE id = $1`, id); got != "working" {
		t.Fatalf("manager_state with queued send_message = %v, want working", got)
	}
	sendCmd := r.take("send_message")
	r.succeed(sendCmd, `{}`)
	if got := r.one(`SELECT manager_state FROM grimm_session_status WHERE id = $1`, id); got != "idle" {
		t.Fatalf("manager_state after send_message succeeded (still idle agent) = %v, want idle", got)
	}

	// Once the manager replies to the human's turn (2nd user_message onwards), its reply shows
	// in grimm_thread while the earlier warm-up entries stay hidden.
	if err := r.reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "mgr-1", Epoch: "e1",
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 4, SeqEnd: 4, At: now.Add(5 * time.Second), Item: json.RawMessage(`{"type":"user_message","text":"Hello Grimm"}`)},
			{SeqStart: 5, SeqEnd: 5, At: now.Add(6 * time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"Hi! How can I help?"}`)},
		},
	}, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT count(*) FROM grimm_thread WHERE session_id = $1 AND kind IN ('assistant_message','tool_call')`, id); got != int64(1) {
		t.Fatalf("post-human manager thread entries = %v, want 1 (warm-up must stay hidden)", got)
	}
	if got := r.one(`SELECT body FROM grimm_thread WHERE session_id = $1 AND kind = 'assistant_message'`, id); got != "Hi! How can I help?" {
		t.Fatalf("visible assistant reply = %v, want Hi! How can I help?", got)
	}
}

func TestGrimmThreadIncludesReasoningEntries(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready()
	ctx := t.Context()
	now := time.Now().UTC()
	if err := r.reg.StoreTranscript(ctx, hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "mgr", Epoch: "e1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"reasoning","text":"Checking the repo layout"}`)},
			{SeqStart: 2, SeqEnd: 2, At: now.Add(time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"Here is the plan."}`)},
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT count(*) FROM grimm_thread WHERE session_id = $1 AND kind = 'reasoning'`, id); got != int64(1) {
		t.Fatalf("reasoning rows in grimm_thread = %v, want 1", got)
	}
	if got := r.one(`SELECT body FROM grimm_thread WHERE session_id = $1 AND kind = 'reasoning'`, id); got != "Checking the repo layout" {
		t.Fatalf("reasoning body = %v", got)
	}
}

func TestFastWakeupDebounceByManagerProvider(t *testing.T) {
	t.Parallel()
	if got := updateDebounceFor("claude"); got != 750*time.Millisecond {
		t.Errorf("claude debounce = %v, want 750ms", got)
	}
	if got := updateDebounceFor("codex"); got != 750*time.Millisecond {
		t.Errorf("codex debounce = %v, want 750ms", got)
	}
	if got := updateDebounceFor("antigravity"); got != 2*time.Second {
		t.Errorf("antigravity debounce = %v, want 2s", got)
	}

	r := newRig(t)
	id := r.ready()
	r.setAgent("mgr", "idle")
	ctx := t.Context()
	if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Name: "athena", Role: "builder", Task: "build it"}); err != nil {
		t.Fatal(err)
	}
	r.succeed(r.take("create_agent"), `{"agentId":"w1"}`)
	r.reconcile(id)
	r.setAgent("w1", "running")
	r.reconcile(id)
	r.setAgent("w1", "idle")
	r.reconcile(id)

	// Immediately after wakeup: not yet flushed (0.75s debounce for Claude manager).
	if cs := r.commands(); len(cs) != 0 {
		t.Fatalf("flushed before 750ms debounce: %+v", cs)
	}
	// After 800ms: flushed! (Would NOT have flushed under the old 2s debounce.)
	r.later(800 * time.Millisecond)
	r.reconcile(id)
	upd := r.take("send_message")
	if !strings.Contains(upd.Args["text"].(string), "athena") {
		t.Fatalf("expected wakeup for athena, got %+v", upd.Args)
	}
}

func TestManagerPromptIsTightAndKeepsStartHere(t *testing.T) {
	t.Parallel()
	p := managerPrompt(Session{Repo: "/w/app", Branch: "grimm/1234", Host: "host-1", MaxWorkers: 20}, []string{"rule 1"}, []string{"build it"})
	if !strings.Contains(p, "150 words") {
		t.Error("managerPrompt missing ~150 word concise brief guidance")
	}
	if !strings.Contains(strings.ToLower(p), "1-line status") && !strings.Contains(strings.ToLower(p), "one-line status") {
		t.Error("managerPrompt missing 1-line status note before tool batches guidance")
	}
	if strings.Contains(strings.ToLower(p), "skip start-here") || strings.Contains(strings.ToLower(p), "do not read start-here") {
		t.Error("managerPrompt must NOT instruct skipping start-here")
	}
}

func TestSupervisorAutoArchivesIdleFinishedWorkers(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready()
	ctx := t.Context()
	if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Name: "athena", Role: "builder", Task: "build it"}); err != nil {
		t.Fatal(err)
	}
	r.succeed(r.take("create_agent"), `{"agentId":"w1"}`)
	r.reconcile(id)
	r.setAgent("w1", "running")
	r.reconcile(id)
	r.setAgent("w1", "idle")
	r.reconcile(id)
	// Deliver wakeup so no pending wakeup blocks auto-archive.
	r.setAgent("mgr", "idle")
	r.later(time.Second)
	r.reconcile(id)
	r.succeed(r.take("send_message"), `{}`)

	// Before timeout: still idle.
	r.later(IdleWorkerArchiveAfter - 2*time.Minute)
	r.reconcile(id)
	if got := r.one(`SELECT status FROM grimm_workers WHERE session_id = $1 AND name = 'athena'`, id); got != "idle" {
		t.Fatalf("worker status before timeout = %v, want idle", got)
	}

	// After timeout: auto-archived and archive_agent enqueued.
	r.later(3 * time.Minute)
	r.reconcile(id)
	if got := r.one(`SELECT status FROM grimm_workers WHERE session_id = $1 AND name = 'athena'`, id); got != "archived" {
		t.Fatalf("worker status after timeout = %v, want archived", got)
	}
	arch := r.take("archive_agent")
	if arch.Args["agentId"] != "w1" {
		t.Fatalf("auto-archive command = %+v, want agentId w1", arch.Args)
	}
}
