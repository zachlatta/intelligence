package paseo_test

import (
	"strings"
	"testing"

	. "github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestCreateWorkspaceFromADirectory(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	w, err := c.CreateWorkspace(ctx5(t), CreateWorkspace{Cwd: "/w/app", Title: "Docs", IdempotencyKey: "cmd-1"})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID == "" || w.Directory != "/w/app" {
		t.Errorf("workspace = %+v", w)
	}
	req := f.Requests("workspace.create.request")[0]
	src := req["source"].(map[string]any)
	if src["kind"] != "directory" || src["path"] != "/w/app" || req["title"] != "Docs" || req["idempotencyKey"] != "cmd-1" {
		t.Errorf("request = %v", req)
	}
}

func TestCreateWorkspaceAsAWorktree(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if _, err := c.CreateWorkspace(ctx5(t), CreateWorkspace{Cwd: "/w/app", Worktree: true, Branch: "fix-login", Base: "main",
		IdempotencyKey: "cmd-2"}); err != nil {
		t.Fatal(err)
	}
	src := f.Requests("workspace.create.request")[0]["source"].(map[string]any)
	if src["kind"] != "worktree" || src["cwd"] != "/w/app" || src["action"] != "branch-off" || src["branchName"] != "fix-login" ||
		src["baseBranch"] != "main" {
		t.Errorf("source = %v", src)
	}
}

func TestCreateWorkspaceTheSameKeyTwiceMakesOne(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	a, err := c.CreateWorkspace(ctx5(t), CreateWorkspace{Cwd: "/w/app", IdempotencyKey: "cmd-1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateWorkspace(ctx5(t), CreateWorkspace{Cwd: "/w/app", IdempotencyKey: "cmd-1"})
	if err != nil || b.ID != a.ID {
		t.Fatalf("second create = %+v, %v; want the first, %s", b, err, a.ID)
	}
}

func TestCreateAgent(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	a, err := c.CreateAgent(ctx5(t), CreateAgent{WorkspaceID: "wks_1", Cwd: "/w/wks_1", Provider: "claude", Model: "claude-opus-5-5",
		Thinking: "high", Mode: "bypassPermissions", Title: "Fix login", Prompt: "fix the login bug",
		Labels: map[string]string{"intelligence.command": "cmd-3"}, IdempotencyKey: "cmd-3"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.WorkspaceID != "wks_1" {
		t.Errorf("agent = %+v", a)
	}
	req := f.Requests("agent.create.request")[0]
	cfg := req["config"].(map[string]any)
	if cfg["provider"] != "claude" || cfg["cwd"] != "/w/wks_1" || cfg["model"] != "claude-opus-5-5" || cfg["thinkingOptionId"] != "high" ||
		cfg["modeId"] != "bypassPermissions" || cfg["title"] != "Fix login" {
		t.Errorf("config = %v", cfg)
	}
	if req["workspaceId"] != "wks_1" || req["initialPrompt"] != "fix the login bug" || req["idempotencyKey"] != "cmd-3" {
		t.Errorf("request = %v", req)
	}
	if l := req["labels"].(map[string]any); l["intelligence.command"] != "cmd-3" {
		t.Errorf("labels = %v", l)
	}
}

func TestDaemonRefusalsAreErrors(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if _, err := c.CreateAgent(ctx5(t), CreateAgent{Cwd: "/w/x", Provider: "nope", IdempotencyKey: "k"}); err == nil ||
		!strings.Contains(err.Error(), "provider") {
		t.Errorf("unknown provider: err = %v", err)
	}
	if err := c.ArchiveWorkspace(ctx5(t), "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("archive missing workspace: err = %v", err)
	}
	if err := c.SetWorkspaceTitle(ctx5(t), "missing", "x"); err == nil {
		t.Errorf("title of a missing workspace: no error")
	}
	if err := c.RenameAgent(ctx5(t), "missing", "x"); err == nil {
		t.Errorf("rename a missing agent: no error")
	}
	if err := c.SendMessage(ctx5(t), "missing", "hi", "m1"); err == nil {
		t.Errorf("message a missing agent: no error")
	}
}

func TestWorkspaceAndAgentActions(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetWorkspaces(paseotest.Workspace("wks_1", "worktree", "done"))
	f.SetAgents(paseotest.Agent("a1", "wks_1", "idle"))
	c := dial(t, f)
	ctx := ctx5(t)
	for name, err := range map[string]error{
		"title":   c.SetWorkspaceTitle(ctx, "wks_1", "Login fix"),
		"rename":  c.RenameAgent(ctx, "a1", "Fixer"),
		"message": c.SendMessage(ctx, "a1", "keep going", "cmd-9"),
		"cancel":  c.CancelAgent(ctx, "a1"),
		"archA":   c.ArchiveAgent(ctx, "a1"),
		"archW":   c.ArchiveWorkspace(ctx, "wks_1"),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r := f.Requests("workspace.title.set.request")[0]; r["workspaceId"] != "wks_1" || r["title"] != "Login fix" {
		t.Errorf("title request = %v", r)
	}
	if r := f.Requests("update_agent_request")[0]; r["agentId"] != "a1" || r["name"] != "Fixer" {
		t.Errorf("rename request = %v", r)
	}
	if r := f.Requests("send_agent_message_request")[0]; r["agentId"] != "a1" || r["text"] != "keep going" || r["messageId"] != "cmd-9" {
		t.Errorf("message request = %v", r)
	}
	if r := f.Requests("cancel_agent_request")[0]; r["agentId"] != "a1" {
		t.Errorf("cancel request = %v", r)
	}
	if r := f.Requests("archive_agent_request")[0]; r["agentId"] != "a1" {
		t.Errorf("archive agent request = %v", r)
	}
	if r := f.Requests("archive_workspace_request")[0]; r["workspaceId"] != "wks_1" {
		t.Errorf("archive workspace request = %v", r)
	}
}
