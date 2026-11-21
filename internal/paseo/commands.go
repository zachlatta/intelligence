package paseo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The requests below change the daemon: they create, rename and archive workspaces and agents, and
// send agents messages. Each fails with the daemon's own reason when it refuses.

// CreateWorkspace is a workspace to create. IdempotencyKey makes a repeat of the same creation
// (a command delivered twice) return the first workspace instead of making a second.
type CreateWorkspace struct {
	Cwd            string // the directory, or for a worktree the repository it is cut from
	Worktree       bool   // a new git worktree on a new Branch, instead of the directory itself
	Branch         string // the worktree's new branch; "" lets the daemon name it
	Base           string // the ref the branch starts from; "" is the repository's default
	Title          string
	IdempotencyKey string
}

// ErrWorkspaceCreationKeyConflict means the daemon already has a different request for this key.
var ErrWorkspaceCreationKeyConflict = errors.New("workspace_request_key_conflict")

// CreateWorkspace creates a workspace and returns it.
func (c *Client) CreateWorkspace(ctx context.Context, w CreateWorkspace) (Workspace, error) {
	if w.Cwd == "" {
		return Workspace{}, errors.New("create workspace: no directory")
	}
	src := map[string]any{"kind": "directory", "path": w.Cwd}
	if w.Worktree {
		src = map[string]any{"kind": "worktree", "cwd": w.Cwd, "action": "branch-off"}
		if w.Branch != "" {
			src["branchName"] = w.Branch
		}
		if w.Base != "" {
			src["baseBranch"] = w.Base
		}
	}
	req := map[string]any{"type": "workspace.create.request", "source": src}
	if w.Title != "" {
		req["title"] = w.Title
	}
	if w.IdempotencyKey != "" {
		req["idempotencyKey"] = w.IdempotencyKey
	}
	var p struct {
		Workspace *Workspace `json:"workspace"`
	}
	if err := c.command(ctx, req, &p); err != nil {
		if err.Error() == "workspace.create.request: "+ErrWorkspaceCreationKeyConflict.Error() {
			return Workspace{}, fmt.Errorf("workspace.create.request: %w", ErrWorkspaceCreationKeyConflict)
		}
		return Workspace{}, err
	}
	if p.Workspace == nil {
		return Workspace{}, errors.New("workspace.create: the daemon returned no workspace")
	}
	return *p.Workspace, nil
}

// WorkspaceCreation reads the completed workspace for a creation's idempotency key.
func (c *Client) WorkspaceCreation(ctx context.Context, key string) (Workspace, error) {
	var p struct {
		Snapshot *struct {
			Phase     string     `json:"phase"`
			Workspace *Workspace `json:"workspace"`
		} `json:"snapshot"`
	}
	req := map[string]any{"type": "creation.subscribe.request", "kind": "workspace", "idempotencyKey": key, "subscribe": false}
	if err := c.command(ctx, req, &p); err != nil {
		return Workspace{}, err
	}
	if p.Snapshot == nil || p.Snapshot.Phase != "completed" || p.Snapshot.Workspace == nil {
		return Workspace{}, ErrWorkspaceCreationKeyConflict
	}
	return *p.Snapshot.Workspace, nil
}

// CreateAgent is an agent to start. IdempotencyKey makes a repeat return the first agent.
type CreateAgent struct {
	WorkspaceID string // the workspace it runs in; "" lets the daemon pick or make one for Cwd
	Cwd         string // its working directory, in the workspace
	Provider    string // claude, codex, ...
	Model       string
	Thinking    string // thinking option id
	Mode        string // provider mode id (permissions)
	Title       string
	Prompt      string // the first message, sent once it starts
	Labels      map[string]string

	IdempotencyKey string
}

// CreateAgent starts an agent and returns it.
func (c *Client) CreateAgent(ctx context.Context, a CreateAgent) (Agent, error) {
	if a.Cwd == "" || a.Provider == "" {
		return Agent{}, errors.New("create agent: a directory and a provider are needed")
	}
	cfg := map[string]any{"provider": a.Provider, "cwd": a.Cwd}
	for k, v := range map[string]string{"model": a.Model, "thinkingOptionId": a.Thinking, "modeId": a.Mode, "title": a.Title} {
		if v != "" {
			cfg[k] = v
		}
	}
	labels := a.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	req := map[string]any{"type": "agent.create.request", "config": cfg, "labels": labels}
	if a.WorkspaceID != "" {
		req["workspaceId"] = a.WorkspaceID
	}
	if a.Prompt != "" {
		req["initialPrompt"] = a.Prompt
	}
	if a.IdempotencyKey != "" {
		req["idempotencyKey"] = a.IdempotencyKey
	}
	var p struct {
		Agent *Agent `json:"agent"`
	}
	if err := c.command(ctx, req, &p); err != nil {
		return Agent{}, err
	}
	if p.Agent == nil {
		return Agent{}, errors.New("agent.create: the daemon returned no agent")
	}
	return *p.Agent, nil
}

// ArchiveWorkspace archives a workspace (and, for a Paseo worktree, removes it).
func (c *Client) ArchiveWorkspace(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "archive_workspace_request", "workspaceId": id}, nil)
}

// ArchiveAgent stops and archives an agent.
func (c *Client) ArchiveAgent(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "archive_agent_request", "agentId": id}, nil)
}

// RefreshAgent loads the same persisted agent, unarchiving it if needed. Modern
// clients must explicitly recover the owning workspace first. It does not start a turn.
func (c *Client) RefreshAgent(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "refresh_agent_request", "agentId": id}, nil)
}

// InspectAgent includes archived persistence records, unlike Agents' live subscription.
func (c *Client) InspectAgent(ctx context.Context, id string) (*Agent, error) {
	var p struct {
		Agent *Agent `json:"agent"`
	}
	err := c.command(ctx, map[string]any{"type": "fetch_agent_request", "agentId": id}, &p)
	return p.Agent, err
}

// WorkspaceRecovery is the daemon's durable recovery plan, without workspace secrets.
type WorkspaceRecovery struct {
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (c *Client) InspectWorkspaceRecovery(ctx context.Context, id string) (WorkspaceRecovery, error) {
	var p struct {
		State WorkspaceRecovery `json:"state"`
	}
	err := c.command(ctx, map[string]any{"type": "workspace.recovery.inspect.request", "workspaceId": id}, &p)
	return p.State, err
}

// RestoreWorkspace recreates an archived worktree and unarchives its workspace.
func (c *Client) RestoreWorkspace(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "workspace.recovery.restore.request", "workspaceId": id}, nil)
}

// SetWorkspaceTitle renames a workspace; "" goes back to the name the daemon derives.
func (c *Client) SetWorkspaceTitle(ctx context.Context, id, title string) error {
	var t any = title
	if title == "" {
		t = nil
	}
	return c.command(ctx, map[string]any{"type": "workspace.title.set.request", "workspaceId": id, "title": t}, nil)
}

// RenameAgent sets an agent's title.
func (c *Client) RenameAgent(ctx context.Context, id, title string) error {
	return c.command(ctx, map[string]any{"type": "update_agent_request", "agentId": id, "name": title}, nil)
}

// How a message treats a turn the agent is running: Steer joins it (the daemon falls back to
// interrupting when the provider cannot steer), Interrupt stops it and starts a new one. Either
// starts a turn on an agent that is not running.
const (
	Steer     = "steer"
	Interrupt = "interrupt"
)

// SendMessage sends an agent a message. messageID makes a repeat of the same message a no-op.
// turn is Steer, Interrupt or "" (the daemon's default, which interrupts).
func (c *Client) SendMessage(ctx context.Context, agentID, text, messageID, turn string) error {
	req := map[string]any{"type": "send_agent_message_request", "agentId": agentID, "text": text}
	if messageID != "" {
		req["messageId"] = messageID
	}
	switch turn {
	case "":
	case Steer, Interrupt:
		req["activeTurnBehavior"] = turn
	default:
		return fmt.Errorf("send message: unknown turn behavior %q", turn)
	}
	return c.command(ctx, req, nil)
}

// CancelAgent stops an agent's current turn.
func (c *Client) CancelAgent(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "cancel_agent_request", "agentId": id}, nil)
}

// command is a request whose answer says in its payload whether the daemon did it: a non-empty
// error, or accepted or ok false, is a refusal.
func (c *Client) command(ctx context.Context, msg map[string]any, out any) error {
	typ := fmt.Sprint(msg["type"])
	var raw json.RawMessage
	if err := c.request(ctx, msg, &raw); err != nil {
		return err
	}
	var p struct {
		Error    *string `json:"error"`
		Accepted *bool   `json:"accepted"`
		OK       *bool   `json:"ok"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("%s: %w", typ, err)
	}
	switch {
	case p.Error != nil && *p.Error != "":
		return fmt.Errorf("%s: %s", typ, *p.Error)
	case p.Accepted != nil && !*p.Accepted, p.OK != nil && !*p.OK:
		return fmt.Errorf("%s: refused by the daemon", typ)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
