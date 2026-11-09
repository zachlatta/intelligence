package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestParseCommandChecksEachKind(t *testing.T) {
	good := map[string]string{
		CreateWorkspaceKind:            `{"cwd":"/w/app","worktree":true,"branch":"fix","base":"main","title":"Fix","agent":{"provider":"claude","prompt":"go"}}`,
		CreateAgentKind:                `{"workspaceId":"w1","provider":"claude","prompt":"go"}`,
		ArchiveWorkspaceKind:           `{"workspaceId":"w1"}`,
		RenameWorkspaceKind:            `{"workspaceId":"w1","title":"New"}`,
		ArchiveAgentKind:               `{"agentId":"a1"}`,
		CancelAgentKind:                `{"agentId":"a1"}`,
		RenameAgentKind:                `{"agentId":"a1","title":"New"}`,
		SendMessageKind:                `{"agentId":"a1","text":"hi"}`,
		SendMessageKind + " steer":     `{"agentId":"a1","text":"hi","behavior":"steer"}`,
		SendMessageKind + " interrupt": `{"agentId":"a1","text":"hi","behavior":"interrupt"}`,
		SendMessageKind + " queue":     `{"agentId":"a1","text":"hi","behavior":"queue"}`,
	}
	for name, args := range good {
		kind, _, _ := strings.Cut(name, " ")
		if _, err := ParseCommand(kind, json.RawMessage(args)); err != nil {
			t.Errorf("%s %s: %v", kind, args, err)
		}
	}
	bad := map[string]string{
		CreateWorkspaceKind + " no cwd":            `{}`,
		CreateWorkspaceKind + " branch":            `{"cwd":"/w","branch":"x"}`,
		CreateWorkspaceKind + " agent no provider": `{"cwd":"/w","agent":{"prompt":"go"}}`,
		CreateWorkspaceKind + " agent elsewhere":   `{"cwd":"/w","agent":{"provider":"claude","cwd":"/x"}}`,
		CreateWorkspaceKind + " agent in another":  `{"cwd":"/w","agent":{"provider":"claude","workspaceId":"w1"}}`,
		CreateAgentKind + " no provider":           `{"cwd":"/w"}`,
		CreateAgentKind + " nowhere":               `{"provider":"claude"}`,
		CreateAgentKind + " unknown field":         `{"provider":"claude","cwd":"/w","env":{"X":"1"}}`,
		ArchiveWorkspaceKind + " no id":            `{}`,
		ArchiveAgentKind + " text":                 `{"agentId":"a1","text":"x"}`,
		RenameAgentKind + " no title":              `{"agentId":"a1"}`,
		SendMessageKind + " no text":               `{"agentId":"a1"}`,
		SendMessageKind + " unknown behavior":      `{"agentId":"a1","text":"hi","behavior":"later"}`,
		CancelAgentKind + " behavior":              `{"agentId":"a1","behavior":"queue"}`,
		"delete_everything x":                      `{}`,
		CreateWorkspaceKind + " not an object":     `[1]`,
	}
	for name, args := range bad {
		kind, _, _ := strings.Cut(name, " ")
		if _, err := ParseCommand(kind, json.RawMessage(args)); err == nil {
			t.Errorf("%s %s: no error", name, args)
		}
	}
}

func execute(t *testing.T, w *Watcher, kind, args string) (CommandResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.Execute(ctx, Command{ID: "cmd-" + kind, Kind: kind, Args: json.RawMessage(args)})
}

func TestExecuteCreatesAnAgentInAWorkspacesDirectory(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	res, err := execute(t, w, CreateAgentKind, `{"workspaceId":"w1","provider":"claude","prompt":"go","labels":{"team":"x"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.AgentID == "" || res.WorkspaceID != "w1" {
		t.Errorf("result = %+v", res)
	}
	req := d.Requests("agent.create.request")[0]
	if cwd := req["config"].(map[string]any)["cwd"]; cwd != "/w/w1" {
		t.Errorf("cwd = %v, want the workspace's directory", cwd)
	}
	labels := req["labels"].(map[string]any)
	if labels["intelligence.command"] != "cmd-create_agent" || labels["team"] != "x" || req["idempotencyKey"] != "cmd-create_agent" {
		t.Errorf("request = %v: the command id is the idempotency key and a label", req)
	}
	eventually(t, w, "the new agent", func(s PaseoStatus) bool { return len(s.Agents) == 3 })
}

func TestExecuteCreatesAWorkspaceWithAnAgentInIt(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	args := `{"cwd":"/w/app","worktree":true,"branch":"dark","title":"Dark mode","agent":{"provider":"claude","prompt":"add dark mode","title":"Dark"}}`
	res, err := execute(t, w, CreateWorkspaceKind, args)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkspaceID == "" || res.AgentID == "" {
		t.Fatalf("result = %+v", res)
	}
	req := d.Requests("agent.create.request")[0]
	if req["workspaceId"] != res.WorkspaceID || req["config"].(map[string]any)["cwd"] != "/w/app-worktrees/dark" ||
		req["initialPrompt"] != "add dark mode" {
		t.Errorf("agent request = %v: it runs in the new workspace's directory", req)
	}
	if req["idempotencyKey"] != "cmd-create_workspace:agent" || req["labels"].(map[string]any)["intelligence.command"] != "cmd-create_workspace" {
		t.Errorf("agent request = %v", req)
	}
	again, err := execute(t, w, CreateWorkspaceKind, args) // redelivered
	if err != nil || again != res || len(d.Requests("agent.create.request")) != 2 {
		t.Errorf("a repeat made something new: %+v %v", again, err)
	}
	eventually(t, w, "the new workspace and agent", func(s PaseoStatus) bool { return len(s.Agents) == 3 })
}

func TestAFailedAgentStillReportsTheWorkspaceItsCommandMade(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	res, err := execute(t, w, CreateWorkspaceKind, `{"cwd":"/w/app","agent":{"provider":"nope","prompt":"go"}}`)
	if err == nil || !strings.Contains(err.Error(), "Unknown provider") || res.WorkspaceID == "" || res.AgentID != "" {
		t.Errorf("result = %+v, %v", res, err)
	}
	u := &updates{}
	r := &Runner{Exec: func(context.Context, Command) (CommandResult, error) {
		return CommandResult{WorkspaceID: "w9"}, errors.New("agent: Unknown provider")
	}, Report: u.report}
	got := r.run(context.Background(), Command{ID: "c1"})
	if got.Status != CommandFailed || string(got.Result) != `{"workspaceId":"w9"}` {
		t.Errorf("update = %+v: a failure keeps what was made", got)
	}
}

func TestExecuteEveryOtherKind(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	res, err := execute(t, w, CreateWorkspaceKind, `{"cwd":"/w/app","title":"Docs"}`)
	if err != nil || !strings.HasPrefix(res.WorkspaceID, "wks_") {
		t.Fatalf("create workspace = %+v, %v", res, err)
	}
	for _, c := range [][2]string{
		{RenameWorkspaceKind, `{"workspaceId":"w1","title":"Login"}`},
		{RenameAgentKind, `{"agentId":"a1","title":"Fixer"}`},
		{SendMessageKind, `{"agentId":"a1","text":"more"}`},
		{CancelAgentKind, `{"agentId":"a1"}`},
		{ArchiveAgentKind, `{"agentId":"a2"}`},
		{ArchiveWorkspaceKind, `{"workspaceId":"w2"}`},
	} {
		if _, err := execute(t, w, c[0], c[1]); err != nil {
			t.Errorf("%s: %v", c[0], err)
		}
	}
	if r := d.Requests("send_agent_message_request")[0]; r["messageId"] != "cmd-send_message" {
		t.Errorf("message id = %v, want the command id", r["messageId"])
	}
	eventually(t, w, "archived ones gone", func(s PaseoStatus) bool {
		return len(s.Agents) == 1 && len(s.Workspaces) == 2 // w1 and the new one
	})
}

func TestExecuteReportsTheDaemonsRefusal(t *testing.T) {
	w := watcher(t, daemon(t))
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	if _, err := execute(t, w, ArchiveWorkspaceKind, `{"workspaceId":"nope"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
	if _, err := execute(t, w, CreateAgentKind, `{"workspaceId":"nope","provider":"claude"}`); err == nil {
		t.Errorf("an agent in an unknown workspace: no error")
	}
}

type updates struct {
	mu   sync.Mutex
	got  []CommandUpdate
	fail int // fail this many reports first
}

func (u *updates) report(ctx context.Context, x CommandUpdate) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail > 0 {
		u.fail--
		return errors.New("not connected")
	}
	u.got = append(u.got, x)
	return nil
}

func (u *updates) statuses() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var s []string
	for _, x := range u.got {
		s = append(s, x.ID+":"+x.Status)
	}
	return s
}

func TestRunnerRunsACommandOnceAndAnswersRedeliveries(t *testing.T) {
	var runs int
	var mu sync.Mutex
	u := &updates{}
	r := &Runner{Exec: func(ctx context.Context, c Command) (CommandResult, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return CommandResult{AgentID: "a9"}, nil
	}, Report: u.report, Retry: time.Millisecond}
	ctx := context.Background()
	r.Handle(ctx, Command{ID: "c1", Kind: CreateAgentKind})
	r.Wait()
	r.Handle(ctx, Command{ID: "c1", Kind: CreateAgentKind}) // the result's ack was lost; the server sent it again
	r.Wait()
	if runs != 1 {
		t.Errorf("ran %d times, want once", runs)
	}
	got := strings.Join(u.statuses(), " ")
	if got != "c1:running c1:succeeded c1:succeeded" {
		t.Errorf("updates = %s", got)
	}
	if string(u.got[1].Result) != `{"agentId":"a9"}` {
		t.Errorf("result = %s", u.got[1].Result)
	}
}

func TestRunnerRetriesTheResultUntilTheServerHasIt(t *testing.T) {
	u := &updates{fail: 3}
	r := &Runner{Exec: func(ctx context.Context, c Command) (CommandResult, error) {
		return CommandResult{}, errors.New("boom")
	},
		Report: u.report, Retry: time.Millisecond}
	r.Handle(context.Background(), Command{ID: "c1"})
	r.Wait()
	if got := u.statuses(); len(got) != 1 || got[0] != "c1:failed" || u.got[0].Error != "boom" {
		t.Errorf("updates = %v (the running update is best effort; the failure must arrive)", u.got)
	}
}

func TestRunnerDoesNotStartAnExpiredCommand(t *testing.T) {
	u := &updates{}
	r := &Runner{Exec: func(ctx context.Context, c Command) (CommandResult, error) {
		t.Error("ran an expired command")
		return CommandResult{}, nil
	}, Report: u.report, Now: func() time.Time { return t0 }}
	r.Handle(context.Background(), Command{ID: "c1", ExpiresAt: t0.Add(-time.Second)})
	r.Wait()
	if got := u.statuses(); len(got) != 1 || got[0] != "c1:failed" {
		t.Errorf("updates = %v", got)
	}
}

func TestASentMessageSteersByDefaultOrInterrupts(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	for _, args := range []string{`{"agentId":"a1","text":"one"}`, `{"agentId":"a1","text":"two","behavior":"steer"}`,
		`{"agentId":"a1","text":"three","behavior":"interrupt"}`} {
		if _, err := execute(t, w, SendMessageKind, args); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, r := range d.Requests("send_agent_message_request") {
		got = append(got, r["text"].(string)+":"+r["activeTurnBehavior"].(string))
	}
	if strings.Join(got, " ") != "one:steer two:steer three:interrupt" {
		t.Errorf("sent %v", got)
	}
}

// sendQueued executes a queued message in the background and returns its result.
func sendQueued(w *Watcher, id string, created time.Time, text string) <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := w.Execute(ctx, Command{ID: id, Kind: SendMessageKind, CreatedAt: created,
			Args: json.RawMessage(`{"agentId":"a1","text":"` + text + `","behavior":"queue"}`)})
		done <- err
	}()
	return done
}

func sentTexts(d interface {
	Requests(string) []map[string]any
}) []string {
	var got []string
	for _, r := range d.Requests("send_agent_message_request") {
		got = append(got, r["text"].(string))
	}
	return got
}

func TestAQueuedMessageWaitsForTheTurnToEndAndQueuedOnesGoInOrder(t *testing.T) {
	d := daemon(t) // a1 is running
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	second := sendQueued(w, "q2", t0.Add(time.Second), "second") // arrives first, but was sent later
	time.Sleep(50 * time.Millisecond)
	first := sendQueued(w, "q1", t0, "first")
	time.Sleep(200 * time.Millisecond)
	if got := sentTexts(d); len(got) != 0 {
		t.Fatalf("sent %v while the agent was running", got)
	}

	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a1", "w1", "idle")})
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the first message's turn is running: the second waits for it
	if got := sentTexts(d); strings.Join(got, " ") != "first" {
		t.Fatalf("sent %v, want only the first", got)
	}
	if r := d.Requests("send_agent_message_request")[0]; r["messageId"] != "q1" {
		t.Errorf("message id = %v", r["messageId"])
	}

	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a1", "w1", "idle")})
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := sentTexts(d); strings.Join(got, " ") != "first second" {
		t.Errorf("sent %v", got)
	}
}

func TestAQueuedMessageToAnIdleAgentGoesAtOnce(t *testing.T) {
	d := daemon(t) // a2 is idle
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	if _, err := execute(t, w, SendMessageKind, `{"agentId":"a2","text":"now","behavior":"queue"}`); err != nil {
		t.Fatal(err)
	}
	if r := d.Requests("send_agent_message_request"); len(r) != 1 || r[0]["activeTurnBehavior"] != "steer" {
		t.Errorf("requests = %v", r)
	}
}

func TestAQueuedMessageFailsWhenItsAgentIsGone(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	if _, err := execute(t, w, SendMessageKind, `{"agentId":"nope","text":"x","behavior":"queue"}`); err == nil {
		t.Errorf("queued for an agent the host does not have: no error")
	}
	waiting := sendQueued(w, "q1", t0, "x")
	time.Sleep(100 * time.Millisecond)
	d.Push("agent_update", map[string]any{"kind": "remove", "agentId": "a1"})
	select {
	case err := <-waiting:
		if err == nil || !strings.Contains(err.Error(), "archived") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting for an archived agent")
	}
	if got := sentTexts(d); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

func TestRunnerLetsAQueuedMessageWaitUntilItExpires(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	var deadlines []time.Time
	r := &Runner{Exec: func(ctx context.Context, c Command) (CommandResult, error) {
		d, _ := ctx.Deadline()
		deadlines = append(deadlines, d)
		return CommandResult{}, nil
	}, Report: (&updates{}).report, Timeout: time.Minute}
	r.run(context.Background(), Command{ID: "q", Kind: SendMessageKind, ExpiresAt: expires,
		Args: json.RawMessage(`{"agentId":"a1","text":"x","behavior":"queue"}`)})
	r.run(context.Background(), Command{ID: "s", Kind: SendMessageKind, ExpiresAt: expires,
		Args: json.RawMessage(`{"agentId":"a1","text":"x"}`)})
	if !deadlines[0].Equal(expires) {
		t.Errorf("queued deadline = %v, want its expiry %v", deadlines[0], expires)
	}
	if time.Until(deadlines[1]) > time.Minute {
		t.Errorf("a steering message's deadline = %v, want the runner's timeout", deadlines[1])
	}
}

func TestArchivingAWorkspaceArchivesItsAgentsFirst(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 2 })
	res, err := execute(t, w, ArchiveWorkspaceKind, `{"workspaceId":"w1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkspaceID != "w1" || res.ArchivedAgents != 2 {
		t.Errorf("result = %+v, want w1 with its two agents archived", res)
	}
	var got []string
	for _, r := range d.Requests("archive_agent_request") {
		got = append(got, r["agentId"].(string))
	}
	if strings.Join(got, ",") != "a1,a2" || len(d.Requests("archive_workspace_request")) != 1 {
		t.Errorf("archived agents %v, workspaces %d", got, len(d.Requests("archive_workspace_request")))
	}
	eventually(t, w, "the workspace and its agents gone", func(s PaseoStatus) bool {
		return len(s.Agents) == 0 && len(s.Workspaces) == 1
	})
}

func TestArchivingAWorkspaceSkipsAnAgentAlreadyGone(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 2 })
	d.SetAgents(paseotest.Agent("a2", "w1", "idle")) // a1 archived elsewhere, before the watcher heard
	res, err := execute(t, w, ArchiveWorkspaceKind, `{"workspaceId":"w1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.ArchivedAgents != 1 || len(d.Requests("archive_workspace_request")) != 1 {
		t.Errorf("result = %+v, want a2 archived and then the workspace", res)
	}
}
