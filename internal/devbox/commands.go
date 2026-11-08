package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// A Command is something the server asks a dev box to do to its Paseo daemon. The server queues it
// in Postgres and sends it down the box's stream; the box runs it and streams back CommandUpdates.
// A command can arrive more than once (a stream that dropped before its result was acknowledged),
// so each kind is safe to repeat: creations and messages carry the command's ID to the daemon as
// their idempotency key, and the rest are idempotent by nature.
type Command struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Args      json.RawMessage `json:"args"`
	CreatedAt time.Time       `json:"createdAt,omitzero"`
	ExpiresAt time.Time       `json:"expiresAt,omitzero"` // a box does not start a command after this
}

// CommandUpdate is a command's progress: running, then succeeded (with Result) or failed (with Error).
type CommandUpdate struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Command statuses. Queued, sent and expired are the server's; a box reports the others.
const (
	CommandQueued    = "queued"
	CommandSent      = "sent"
	CommandRunning   = "running"
	CommandSucceeded = "succeeded"
	CommandFailed    = "failed"
	CommandExpired   = "expired"
)

// The command kinds and their arguments.
const (
	CreateWorkspaceKind  = "create_workspace"
	ArchiveWorkspaceKind = "archive_workspace"
	RenameWorkspaceKind  = "rename_workspace"
	CreateAgentKind      = "create_agent"
	ArchiveAgentKind     = "archive_agent"
	RenameAgentKind      = "rename_agent"
	SendMessageKind      = "send_message"
	CancelAgentKind      = "cancel_agent"
)

// CreateWorkspaceArgs: a workspace on Cwd, or with Worktree a new git worktree of the repository at
// Cwd on a new Branch from Base. With Agent, an agent is then started in it (Agent names no
// workspace or directory of its own).
type CreateWorkspaceArgs struct {
	Cwd      string           `json:"cwd"`
	Worktree bool             `json:"worktree,omitempty"`
	Branch   string           `json:"branch,omitempty"`
	Base     string           `json:"base,omitempty"`
	Title    string           `json:"title,omitempty"`
	Agent    *CreateAgentArgs `json:"agent,omitempty"`
}

// CreateAgentArgs: an agent in WorkspaceID (its directory unless Cwd is given), or in Cwd.
type CreateAgentArgs struct {
	WorkspaceID string            `json:"workspaceId,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Provider    string            `json:"provider"`
	Model       string            `json:"model,omitempty"`
	Thinking    string            `json:"thinking,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Title       string            `json:"title,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// WorkspaceArgs name a workspace: archive_workspace, and rename_workspace with Title ("" resets it).
type WorkspaceArgs struct {
	WorkspaceID string `json:"workspaceId"`
	Title       string `json:"title,omitempty"`
}

// AgentArgs name an agent: archive_agent, cancel_agent, rename_agent with Title, send_message with Text.
type AgentArgs struct {
	AgentID string `json:"agentId"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text,omitempty"`
}

// CommandResult is what a command made or changed.
type CommandResult struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	AgentID     string `json:"agentId,omitempty"`
}

// ParseCommand decodes args for kind strictly (no unknown fields) and checks what it needs. The
// server refuses a command that does not parse, so a box never receives one.
func ParseCommand(kind string, args json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	decode := func(v any) error {
		d := json.NewDecoder(bytes.NewReader(args))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return fmt.Errorf("%s args: %w", kind, err)
		}
		return nil
	}
	switch kind {
	case CreateWorkspaceKind:
		var a CreateWorkspaceArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.Cwd == "" {
			return nil, errors.New("create_workspace needs cwd")
		}
		if !a.Worktree && (a.Branch != "" || a.Base != "") {
			return nil, errors.New("create_workspace: branch and base are for a worktree")
		}
		if a.Agent != nil && a.Agent.Provider == "" {
			return nil, errors.New("create_workspace: its agent needs a provider")
		}
		if a.Agent != nil && (a.Agent.WorkspaceID != "" || a.Agent.Cwd != "") {
			return nil, errors.New("create_workspace: its agent runs in the new workspace, so it takes no workspaceId or cwd")
		}
		return a, nil
	case CreateAgentKind:
		var a CreateAgentArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.Provider == "" || (a.WorkspaceID == "" && a.Cwd == "") {
			return nil, errors.New("create_agent needs a provider, and a workspaceId or cwd")
		}
		return a, nil
	case ArchiveWorkspaceKind, RenameWorkspaceKind:
		var a WorkspaceArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.WorkspaceID == "" {
			return nil, fmt.Errorf("%s needs workspaceId", kind)
		}
		if kind == ArchiveWorkspaceKind && a.Title != "" {
			return nil, errors.New("archive_workspace takes no title")
		}
		return a, nil
	case ArchiveAgentKind, CancelAgentKind, RenameAgentKind, SendMessageKind:
		var a AgentArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		switch {
		case a.AgentID == "":
			return nil, fmt.Errorf("%s needs agentId", kind)
		case kind == RenameAgentKind && a.Title == "":
			return nil, errors.New("rename_agent needs title")
		case kind == SendMessageKind && a.Text == "":
			return nil, errors.New("send_message needs text")
		case kind != RenameAgentKind && a.Title != "", kind != SendMessageKind && a.Text != "":
			return nil, fmt.Errorf("%s takes only agentId", kind)
		}
		return a, nil
	}
	return nil, fmt.Errorf("unknown command kind %q", kind)
}

// Execute runs cmd against the daemon on the watcher's connection.
func (w *Watcher) Execute(ctx context.Context, cmd Command) (CommandResult, error) {
	parsed, err := ParseCommand(cmd.Kind, cmd.Args)
	if err != nil {
		return CommandResult{}, err
	}
	w.mu.Lock()
	c := w.client
	w.mu.Unlock()
	if c == nil {
		return CommandResult{}, errors.New("not connected to the Paseo daemon")
	}
	switch a := parsed.(type) {
	case CreateWorkspaceArgs:
		ws, err := c.CreateWorkspace(ctx, paseo.CreateWorkspace{Cwd: a.Cwd, Worktree: a.Worktree, Branch: a.Branch, Base: a.Base,
			Title: a.Title, IdempotencyKey: cmd.ID})
		if err != nil || a.Agent == nil {
			return CommandResult{WorkspaceID: ws.ID}, err
		}
		ag := *a.Agent
		ag.WorkspaceID, ag.Cwd = ws.ID, ws.Directory
		// its own idempotency key: a repeat returns the same agent, never the workspace
		res, err := createAgent(ctx, c, ag, cmd.ID, cmd.ID+":agent")
		res.WorkspaceID = ws.ID
		if err != nil {
			err = fmt.Errorf("made workspace %s, but its agent: %w", ws.ID, err)
		}
		return res, err
	case CreateAgentArgs:
		if a.Cwd == "" {
			w.mu.Lock()
			a.Cwd = w.workspaces[a.WorkspaceID].Directory
			w.mu.Unlock()
			if a.Cwd == "" {
				return CommandResult{}, fmt.Errorf("no workspace %s on this box", a.WorkspaceID)
			}
		}
		return createAgent(ctx, c, a, cmd.ID, cmd.ID)
	case WorkspaceArgs:
		if cmd.Kind == ArchiveWorkspaceKind {
			return CommandResult{WorkspaceID: a.WorkspaceID}, c.ArchiveWorkspace(ctx, a.WorkspaceID)
		}
		return CommandResult{WorkspaceID: a.WorkspaceID}, c.SetWorkspaceTitle(ctx, a.WorkspaceID, a.Title)
	case AgentArgs:
		r := CommandResult{AgentID: a.AgentID}
		switch cmd.Kind {
		case ArchiveAgentKind:
			return r, c.ArchiveAgent(ctx, a.AgentID)
		case CancelAgentKind:
			return r, c.CancelAgent(ctx, a.AgentID)
		case RenameAgentKind:
			return r, c.RenameAgent(ctx, a.AgentID, a.Title)
		case SendMessageKind:
			return r, c.SendMessage(ctx, a.AgentID, a.Text, cmd.ID)
		}
	}
	return CommandResult{}, fmt.Errorf("unknown command kind %q", cmd.Kind)
}

// createAgent starts a in a.Cwd, labelled with the command that asked for it.
func createAgent(ctx context.Context, c *paseo.Client, a CreateAgentArgs, cmdID, key string) (CommandResult, error) {
	labels := map[string]string{"intelligence.command": cmdID}
	for k, v := range a.Labels {
		labels[k] = v
	}
	ag, err := c.CreateAgent(ctx, paseo.CreateAgent{WorkspaceID: a.WorkspaceID, Cwd: a.Cwd, Provider: a.Provider, Model: a.Model,
		Thinking: a.Thinking, Mode: a.Mode, Title: a.Title, Prompt: a.Prompt, Labels: labels, IdempotencyKey: key})
	return CommandResult{AgentID: ag.ID, WorkspaceID: ag.WorkspaceID}, err
}

// Runner runs the commands a box receives, each once: a command delivered again while it runs is
// ignored, and one delivered again after it finished gets its result reported again rather than
// running twice. Results are reported until the server acknowledges them.
type Runner struct {
	Exec    func(ctx context.Context, cmd Command) (CommandResult, error)
	Report  func(ctx context.Context, u CommandUpdate) error // fails while the box is not connected
	Now     func() time.Time
	Timeout time.Duration // per command; default 5 minutes
	Retry   time.Duration // between attempts to report a result; default 2s

	mu       sync.Mutex
	running  map[string]bool
	finished map[string]CommandUpdate
	order    []string // finished, oldest first, to bound the map
	wg       sync.WaitGroup
}

// keepFinished is how many finished commands a runner remembers to answer redeliveries.
const keepFinished = 1000

// Handle starts cmd unless it is running or finished. It returns at once.
func (r *Runner) Handle(ctx context.Context, cmd Command) {
	r.mu.Lock()
	if r.running == nil {
		r.running, r.finished = map[string]bool{}, map[string]CommandUpdate{}
	}
	if r.running[cmd.ID] {
		r.mu.Unlock()
		return
	}
	done, ok := r.finished[cmd.ID]
	if !ok {
		r.running[cmd.ID] = true
	}
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if ok {
			r.report(ctx, done)
			return
		}
		u := r.run(ctx, cmd)
		r.mu.Lock()
		delete(r.running, cmd.ID)
		r.finished[cmd.ID] = u
		r.order = append(r.order, cmd.ID)
		if len(r.order) > keepFinished {
			delete(r.finished, r.order[0])
			r.order = r.order[1:]
		}
		r.mu.Unlock()
		r.report(ctx, u)
	}()
}

// Wait waits for every command's goroutine to end (tests).
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) run(ctx context.Context, cmd Command) CommandUpdate {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if !cmd.ExpiresAt.IsZero() && now().After(cmd.ExpiresAt) {
		return CommandUpdate{ID: cmd.ID, Status: CommandFailed, Error: "expired before the box could start it"}
	}
	_ = r.Report(ctx, CommandUpdate{ID: cmd.ID, Status: CommandRunning}) // progress only: the result is what must arrive
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := r.Exec(ectx, cmd)
	var b json.RawMessage
	if res != (CommandResult{}) {
		b, _ = json.Marshal(res)
	}
	if err != nil { // keep what it made before it failed: a workspace whose agent did not start
		return CommandUpdate{ID: cmd.ID, Status: CommandFailed, Result: b, Error: err.Error()}
	}
	if b == nil {
		b = json.RawMessage("{}")
	}
	return CommandUpdate{ID: cmd.ID, Status: CommandSucceeded, Result: b}
}

func (r *Runner) report(ctx context.Context, u CommandUpdate) {
	retry := r.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	for ctx.Err() == nil {
		if r.Report(ctx, u) == nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
	}
}
