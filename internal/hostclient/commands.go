package hostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/attachment"
	"github.com/zachlatta/intelligence/internal/paseo"
)

// A Command is something the server asks a host to do to its Paseo daemon. The server queues it
// in Postgres and sends it down the host's stream; the host runs it and streams back CommandUpdates.
// A command can arrive more than once (a stream that dropped before its result was acknowledged),
// so each kind is safe to repeat: creations and messages carry the command's ID to the daemon as
// their idempotency key, and the rest are idempotent by nature. Two are not: the daemon takes no
// idempotency key for creating or running a schedule, so only the Runner's memory of finished
// commands keeps a repeat of those from running twice.
type Command struct {
	IdempotencyKey string          `json:"idempotencyKey,omitempty"` // stable across workflow start retries
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Args           json.RawMessage `json:"args"`
	CreatedAt      time.Time       `json:"createdAt,omitzero"`
	ExpiresAt      time.Time       `json:"expiresAt,omitzero"` // a host does not start a command after this
}

// CommandUpdate is a command's progress: running, then succeeded (with Result) or failed (with Error).
type CommandUpdate struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Command statuses. Queued, sent and expired are the server's; a host reports the others.
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
	Attachments []attachment.Image `json:"attachments,omitempty"`
	WorkspaceID string             `json:"workspaceId,omitempty"`
	Cwd         string             `json:"cwd,omitempty"`
	Provider    string             `json:"provider"`
	Model       string             `json:"model,omitempty"`
	Thinking    string             `json:"thinking,omitempty"`
	Mode        string             `json:"mode,omitempty"`
	Title       string             `json:"title,omitempty"`
	Prompt      string             `json:"prompt,omitempty"`
	Labels      map[string]string  `json:"labels,omitempty"`
}

// WorkspaceArgs name a workspace: archive_workspace, and rename_workspace with Title ("" resets it).
type WorkspaceArgs struct {
	WorkspaceID  string `json:"workspaceId"`
	Title        string `json:"title,omitempty"`
	WaitForAgent string `json:"waitForAgent,omitempty"` // archive: let this agent finish its turn first
}

// AgentArgs name an agent: archive_agent, cancel_agent, rename_agent with Title, send_message with
// Text and, optionally, Behavior.
type AgentArgs struct {
	Attachments []attachment.Image `json:"attachments,omitempty"`
	AgentID     string             `json:"agentId"`
	Title       string             `json:"title,omitempty"`
	Text        string             `json:"text,omitempty"`
	Behavior    string             `json:"behavior,omitempty"` // send_message: steer (""), interrupt or queue
}

// How a message treats the agent's running turn. Each starts a turn when the agent is not running.
const (
	Steer     = "steer"     // joins the running turn (Paseo interrupts when the provider cannot steer)
	Interrupt = "interrupt" // stops the running turn and starts a new one
	Queue     = "queue"     // waits for the running turn, and the messages queued before it, to end
)

// waitsForTurn: a queued message or deferred archive may wait as long as the command lives.
func (c Command) waitsForTurn() bool {
	if c.Kind == ArchiveWorkspaceKind {
		var a WorkspaceArgs
		return json.Unmarshal(c.Args, &a) == nil && a.WaitForAgent != ""
	}
	if c.Kind != SendMessageKind {
		return false
	}
	var a AgentArgs
	return json.Unmarshal(c.Args, &a) == nil && a.Behavior == Queue
}

// CommandResult is what a command made or changed.
type CommandResult struct {
	WorkspaceID    string `json:"workspaceId,omitempty"`
	AgentID        string `json:"agentId,omitempty"`
	ScheduleID     string `json:"scheduleId,omitempty"`
	RunID          string `json:"runId,omitempty"`          // run_schedule: the run it started
	ArchivedAgents int    `json:"archivedAgents,omitempty"` // archive_workspace: how many agents went with it
}

// ParseCommand decodes args for kind strictly (no unknown fields) and checks what it needs. The
// server refuses a command that does not parse, so a host never receives one.
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
		if kind == RenameWorkspaceKind && a.WaitForAgent != "" {
			return nil, errors.New("rename_workspace takes no waitForAgent")
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
		case kind == SendMessageKind && a.Behavior != "" && a.Behavior != Steer && a.Behavior != Interrupt && a.Behavior != Queue:
			return nil, fmt.Errorf("send_message: behavior %q is not steer, interrupt or queue", a.Behavior)
		case kind != SendMessageKind && len(a.Attachments) > 0:
			return nil, fmt.Errorf("%s takes no attachments", kind)
		case kind != SendMessageKind && a.Behavior != "":
			return nil, fmt.Errorf("%s takes no behavior", kind)
		case kind != RenameAgentKind && a.Title != "", kind != SendMessageKind && a.Text != "":
			return nil, fmt.Errorf("%s takes only agentId", kind)
		}
		return a, nil
	case CreateScheduleKind, UpdateScheduleKind, PauseScheduleKind, ResumeScheduleKind, DeleteScheduleKind, RunScheduleKind:
		return parseSchedule(kind, decode) // schedules.go
	}
	return nil, fmt.Errorf("unknown command kind %q", kind)
}

// Execute runs cmd against the daemon on the watcher's connection.
func (w *Watcher) Execute(ctx context.Context, cmd Command) (CommandResult, error) {
	parsed, err := ParseCommand(cmd.Kind, cmd.Args)
	if err != nil {
		return CommandResult{}, err
	}
	if a, ok := parsed.(WorkspaceArgs); ok && cmd.Kind == ArchiveWorkspaceKind && a.WaitForAgent != "" {
		return w.archiveAfterTurn(ctx, a)
	}
	w.mu.Lock()
	c := w.client
	w.mu.Unlock()
	if c == nil {
		return CommandResult{}, errors.New("not connected to the Paseo daemon")
	}
	if IsScheduleKind(cmd.Kind) {
		return executeSchedule(ctx, c, cmd, parsed)
	}
	home, _ := os.UserHomeDir()
	key := cmd.IdempotencyKey
	if key == "" {
		key = cmd.ID
	}
	switch a := parsed.(type) {
	case CreateWorkspaceArgs:
		a.Cwd = expandHome(a.Cwd, home)
		ws, err := c.CreateWorkspace(ctx, paseo.CreateWorkspace{Cwd: a.Cwd, Worktree: a.Worktree, Branch: a.Branch, Base: a.Base,
			Title: a.Title, IdempotencyKey: key})
		if err != nil || a.Agent == nil {
			return CommandResult{WorkspaceID: ws.ID}, err
		}
		ag := *a.Agent
		ag.WorkspaceID, ag.Cwd = ws.ID, ws.Directory
		// its own idempotency key: a repeat returns the same agent, never the workspace
		res, err := createAgent(ctx, c, ag, key, key+":agent")
		res.WorkspaceID = ws.ID
		if err != nil {
			err = fmt.Errorf("made workspace %s, but its agent: %w", ws.ID, err)
		}
		return res, err
	case CreateAgentArgs:
		a.Cwd = expandHome(a.Cwd, home)
		if a.Cwd == "" {
			w.mu.Lock()
			a.Cwd = w.workspaces[a.WorkspaceID].Directory
			w.mu.Unlock()
			if a.Cwd == "" {
				return CommandResult{}, fmt.Errorf("no workspace %s on this host", a.WorkspaceID)
			}
		}
		return createAgent(ctx, c, a, key, key)
	case WorkspaceArgs:
		if cmd.Kind == ArchiveWorkspaceKind {
			return w.archiveWorkspace(ctx, c, a.WorkspaceID)
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
			if a.Behavior != Queue {
				if err := w.prepareMessageImages(a); err != nil {
					return r, err
				}
			}
			switch a.Behavior {
			case Queue:
				return r, w.sendQueued(ctx, cmd, a)
			case Interrupt:
				return r, c.SendMessage(ctx, a.AgentID, a.Text, cmd.ID, paseo.Interrupt)
			}
			return r, c.SendMessage(ctx, a.AgentID, a.Text, cmd.ID, paseo.Steer)
		}
	}
	return CommandResult{}, fmt.Errorf("unknown command kind %q", cmd.Kind)
}

// archiveAfterTurn uses the current connection on each attempt: a daemon restart or
// self-update while the orchestrator finishes must not abandon durable cleanup.
func (w *Watcher) archiveAfterTurn(ctx context.Context, a WorkspaceArgs) (CommandResult, error) {
	for {
		w.mu.Lock()
		c := w.client
		w.mu.Unlock()
		if c != nil {
			agents, err := c.Agents(ctx)
			if err == nil {
				running := false
				for _, ag := range agents {
					if ag.ID == a.WaitForAgent {
						running = busy(ag.Status)
						break
					}
				}
				w.mu.Lock()
				current := w.client
				w.mu.Unlock()
				if !running && current != nil && current == c {
					result, err := w.archiveWorkspace(ctx, current, a.WorkspaceID)
					if err == nil {
						return result, nil
					}
					select {
					case <-current.Done(): // retry after a transport failure; archives are idempotent
					default:
						return result, err
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return CommandResult{WorkspaceID: a.WorkspaceID}, fmt.Errorf("waiting for orchestrator's turn to end: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// archiveWorkspace archives each agent in the workspace, then the workspace. Paseo 0.15 archives a
// workspace's agents itself, but an older daemon leaves them running in a workspace it no longer
// lists, so the host does it first. An agent already gone (archived elsewhere) is skipped.
func (w *Watcher) archiveWorkspace(ctx context.Context, c *paseo.Client, id string) (CommandResult, error) {
	w.mu.Lock()
	var agents []string
	for _, ag := range w.agents {
		if ag.WorkspaceID == id {
			agents = append(agents, ag.ID)
		}
	}
	w.mu.Unlock()
	slices.Sort(agents)
	r := CommandResult{WorkspaceID: id}
	for _, ag := range agents {
		if err := c.ArchiveAgent(ctx, ag); err != nil {
			if strings.Contains(err.Error(), "not found") {
				continue
			}
			return r, fmt.Errorf("archive agent %s: %w", ag, err)
		}
		r.ArchivedAgents++
	}
	return r, c.ArchiveWorkspace(ctx, id)
}

// expandHome reads a leading "~" in path as the home directory: the server does not know it.
func expandHome(path, home string) string {
	if home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}

// createAgent starts a in a.Cwd, labelled with the command that asked for it.
func createAgent(ctx context.Context, c *paseo.Client, a CreateAgentArgs, cmdID, key string) (CommandResult, error) {
	if err := materializeImages(a.Cwd, a.Attachments); err != nil {
		return CommandResult{}, err
	}
	labels := map[string]string{"intelligence.command": cmdID}
	for k, v := range a.Labels {
		labels[k] = v
	}
	ag, err := c.CreateAgent(ctx, paseo.CreateAgent{WorkspaceID: a.WorkspaceID, Cwd: a.Cwd, Provider: a.Provider, Model: a.Model,
		Thinking: a.Thinking, Mode: a.Mode, Title: a.Title, Prompt: a.Prompt, Labels: labels, IdempotencyKey: key})
	return CommandResult{AgentID: ag.ID, WorkspaceID: ag.WorkspaceID}, err
}

// Runner runs the commands a host receives, each once: a command delivered again while it runs is
// ignored, and one delivered again after it finished gets its result reported again rather than
// running twice. Results are reported until the server acknowledges them.
type Runner struct {
	Exec    func(ctx context.Context, cmd Command) (CommandResult, error)
	Report  func(ctx context.Context, u CommandUpdate) error // fails while the host is not connected
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
		return CommandUpdate{ID: cmd.ID, Status: CommandFailed, Error: "expired before the host could start it"}
	}
	_ = r.Report(ctx, CommandUpdate{ID: cmd.ID, Status: CommandRunning}) // progress only: the result is what must arrive
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := now().Add(timeout)
	if cmd.waitsForTurn() && cmd.ExpiresAt.After(deadline) {
		deadline = cmd.ExpiresAt
	}
	ectx, cancel := context.WithDeadline(ctx, deadline)
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
