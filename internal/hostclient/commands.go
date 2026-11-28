package hostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/agenttext"
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
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Args           json.RawMessage `json:"args"`
	CreatedAt      time.Time       `json:"createdAt,omitzero"`
	ExpiresAt      time.Time       `json:"expiresAt,omitzero"`       // a host does not start a command after this
	IdempotencyKey string          `json:"idempotencyKey,omitempty"` // stable across workflow start retries
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
	WatchAgentKind       = "watch_agent" // set the agent's LiveLabel: how fast the host copies its transcript
	SendMessageKind      = "send_message"
	CancelAgentKind      = "cancel_agent"
	ContinueEmailKind    = "continue_email"
	SwitchLoginKind      = "switch_login"
	ReadGitFileKind      = "read_git_file"
)

// ReadGitFileArgs reads a single UTF-8 file from a verified commit on origin's default branch
// (whose file blob matches the current origin default tip) in an enrolled Git repository on the
// host. It refreshes the origin tracking ref without changing the checkout.
type ReadGitFileArgs struct {
	Directory string `json:"directory"`
	Commit    string `json:"commit"`
	Path      string `json:"path"`
}

var gitCommitSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// SwitchLoginArgs: make Login the login the host's Provider tool uses, for every agent of that
// provider on the host. Only Claude, through cswap, can switch. The host finds the login's cswap
// slot when it runs the command, since slots can change. From and Why say what the server saw
// when it asked, for the record.
type SwitchLoginArgs struct {
	Provider string `json:"provider"`
	Login    string `json:"login"`
	From     string `json:"from,omitempty"`
	Why      string `json:"why,omitempty"`
}

// ContinueEmailArgs restores a finished task's agent before sending the new run's prompt.
// AfterCommand serializes restoration after the earlier workspace cleanup on the server.
type ContinueEmailArgs struct {
	RunID        string  `json:"runId,omitempty"`
	AgentID      string  `json:"agentId"`
	Text         string  `json:"text"`
	AfterCommand *string `json:"afterCommand,omitempty"`
}

// CreateWorkspaceArgs: a workspace on Cwd, or with Worktree a new git worktree of the repository at
// Cwd on a new Branch from Base. With Agent, an agent is then started in it (Agent names no
// workspace or directory of its own).
type CreateWorkspaceArgs struct {
	Cwd          string           `json:"cwd"`
	Worktree     bool             `json:"worktree,omitempty"`
	Branch       string           `json:"branch,omitempty"`
	Base         string           `json:"base,omitempty"`
	WorktreeSlug string           `json:"worktreeSlug,omitempty"`
	Title        string           `json:"title,omitempty"`
	Agent        *CreateAgentArgs `json:"agent,omitempty"`
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
	ServiceTier string             `json:"serviceTier,omitempty"` // codex speed (priority, ultrafast)
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
	AfterCommand *string            `json:"afterCommand,omitempty"` // send after this command succeeds
	Attachments  []attachment.Image `json:"attachments,omitempty"`
	AgentID      string             `json:"agentId"`
	Title        string             `json:"title,omitempty"`
	Text         string             `json:"text,omitempty"`
	Behavior     string             `json:"behavior,omitempty"` // send_message: steer (""), interrupt or queue
	Live         string             `json:"live,omitempty"`     // watch_agent: LiveFast while someone watches, else LiveSteady
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

type CommandResult struct {
	NotDelivered         bool   `json:"notDelivered,omitempty"` // failure before calling SendMessage
	FailureStage         string `json:"failureStage,omitempty"`
	AgentGone            bool   `json:"agentGone,omitempty"`
	WorkspaceID          string `json:"workspaceId,omitempty"`
	AgentID              string `json:"agentId,omitempty"`
	ScheduleID           string `json:"scheduleId,omitempty"`
	RunID                string `json:"runId,omitempty"`          // run_schedule: the run it started
	ArchivedAgents       int    `json:"archivedAgents,omitempty"` // archive_workspace: how many agents went with it
	WorktreeBaseFallback bool   `json:"worktreeBaseFallback,omitempty"`
	Login                string `json:"login,omitempty"`   // switch_login: the login now active
	Slot                 int    `json:"slot,omitempty"`    // switch_login: its cswap slot when switched
	Content              string `json:"content,omitempty"` // read_git_file: UTF-8 blob contents at commit
}

// ParseCommand decodes args for kind strictly (no unknown fields) and checks what it needs. The
// server refuses a command that does not parse, so a host never receives one.
func ParseCommand(kind string, args json.RawMessage) (any, error) {
	if err := agenttext.Validate(kind); err != nil {
		return nil, err
	}
	// Decode before checking so JSON escapes cannot hide a credential. Check all
	// string fields, including titles and labels shown alongside agent messages.
	var data any
	if json.NewDecoder(bytes.NewReader(args)).Decode(&data) == nil {
		if err := checkCommandText(data); err != nil {
			return nil, err
		}
	}
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
	case ContinueEmailKind:
		var a ContinueEmailArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.AgentID == "" || a.Text == "" {
			return nil, errors.New("continue_email needs agentId and text")
		}
		return a, nil
	case CreateWorkspaceKind:
		var a CreateWorkspaceArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.Cwd == "" {
			return nil, errors.New("create_workspace needs cwd")
		}
		if !a.Worktree && (a.Branch != "" || a.Base != "" || a.WorktreeSlug != "") {
			return nil, errors.New("create_workspace: branch, base and worktreeSlug are for a worktree")
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
	case ArchiveAgentKind, CancelAgentKind, RenameAgentKind, SendMessageKind, WatchAgentKind:
		var a AgentArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		switch {
		case a.AgentID == "":
			return nil, fmt.Errorf("%s needs agentId", kind)
		case kind != SendMessageKind && a.AfterCommand != nil:
			return nil, fmt.Errorf("%s takes no afterCommand", kind)
		case kind == RenameAgentKind && a.Title == "":
			return nil, errors.New("rename_agent needs title")
		case kind == WatchAgentKind && a.Live != LiveFast && a.Live != LiveSteady:
			return nil, errors.New("watch_agent needs live: fast or steady")
		case kind != WatchAgentKind && a.Live != "":
			return nil, fmt.Errorf("%s takes no live", kind)
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
	case SwitchLoginKind:
		var a SwitchLoginArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.Provider != "claude" || strings.TrimSpace(a.Login) == "" {
			return nil, errors.New("switch_login needs provider claude and a login")
		}
		return a, nil
	case ReadGitFileKind:
		var a ReadGitFileArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		dir := strings.TrimSpace(a.Directory)
		if dir == "" || !path.IsAbs(dir) || slices.Contains(strings.Split(dir, "/"), "..") {
			return nil, errors.New("read_git_file needs a clean absolute repository directory")
		}
		commit := strings.ToLower(strings.TrimSpace(a.Commit))
		if !gitCommitSHARe.MatchString(commit) {
			return nil, errors.New("read_git_file needs a 40-character hex commit SHA")
		}
		rel := strings.TrimSpace(a.Path)
		if rel == "" || path.IsAbs(rel) || strings.Contains(rel, ":") || slices.Contains(strings.Split(rel, "/"), "..") || path.Clean(rel) != rel || rel == "." {
			return nil, errors.New("read_git_file needs a clean repo-relative file path")
		}
		a.Directory = path.Clean(dir)
		a.Commit = commit
		a.Path = rel
		return a, nil
	case CreateScheduleKind, UpdateScheduleKind, PauseScheduleKind, ResumeScheduleKind, DeleteScheduleKind, RunScheduleKind:
		return parseSchedule(kind, decode) // schedules.go
	}
	return nil, fmt.Errorf("unknown command kind %q", kind)
}

func checkCommandText(v any) error {
	switch v := v.(type) {
	case string:
		return agenttext.Validate(v)
	case map[string]any:
		for k, child := range v {
			if err := agenttext.Validate(k); err != nil {
				return err
			}
			if err := checkCommandText(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := checkCommandText(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Watcher) Execute(ctx context.Context, cmd Command) (CommandResult, error) {
	parsed, err := ParseCommand(cmd.Kind, cmd.Args)
	if err != nil {
		return CommandResult{}, err
	}
	if a, ok := parsed.(ReadGitFileArgs); ok {
		content, err := ReadGitFile(ctx, a)
		return CommandResult{Content: content}, err
	}
	if a, ok := parsed.(WorkspaceArgs); ok && cmd.Kind == ArchiveWorkspaceKind && a.WaitForAgent != "" {
		return w.archiveAfterTurn(ctx, a)
	}
	w.mu.Lock()
	c := w.client
	w.mu.Unlock()
	if c == nil {
		if a, ok := parsed.(ContinueEmailArgs); ok {
			return CommandResult{AgentID: a.AgentID, NotDelivered: true, FailureStage: "connect"}, errors.New("not connected to the Paseo daemon")
		}
		return CommandResult{}, errors.New("not connected to the Paseo daemon")
	}
	if IsScheduleKind(cmd.Kind) {
		return executeSchedule(ctx, c, cmd, w.withStartModel(parsed))
	}
	home, _ := os.UserHomeDir()
	key := cmd.IdempotencyKey
	if key == "" {
		key = cmd.ID
	}
	switch a := parsed.(type) {
	case ContinueEmailArgs:
		result := CommandResult{AgentID: a.AgentID}
		beforeSend := func(stage string, err error) (CommandResult, error) {
			result.NotDelivered = true
			result.FailureStage = stage
			return result, err
		}
		ag, err := c.InspectAgent(ctx, a.AgentID)
		if err != nil {
			result.AgentGone = strings.Contains(err.Error(), "Agent not found:")
			return beforeSend("inspect_agent", err)
		}
		if ag == nil {
			result.AgentGone = true
			return beforeSend("inspect_agent", fmt.Errorf("Agent not found: %s", a.AgentID))
		}
		result.WorkspaceID = ag.WorkspaceID
		if ag.WorkspaceID == "" {
			return beforeSend("inspect_workspace", errors.New("agent has no owning workspace"))
		}
		// archive_workspace removes a worktree. Modern Paseo clients receive no
		// implicit recovery from refresh_agent_request, so recover it explicitly.
		recovery, err := c.InspectWorkspaceRecovery(ctx, ag.WorkspaceID)
		if err != nil {
			return beforeSend("inspect_workspace", err)
		}
		switch {
		case recovery.Kind == "recoverable":
			if err := c.RestoreWorkspace(ctx, ag.WorkspaceID); err != nil {
				return beforeSend("restore_workspace", err)
			}
		case recovery.Kind == "unavailable" && recovery.Reason == "workspace_not_archived":
			// Already active, including a replay after an earlier successful send.
		default:
			return beforeSend("inspect_workspace", fmt.Errorf("workspace cannot be recovered (%s): %s", recovery.Reason, recovery.Message))
		}
		if !ag.ArchivedAt.IsZero() {
			if err := c.RefreshAgent(ctx, a.AgentID); err != nil {
				return beforeSend("refresh_agent", err)
			}
		}
		if err := c.SendMessage(ctx, a.AgentID, a.Text, cmd.ID, paseo.Steer); err != nil {
			result.FailureStage = "send_message"
			return result, err
		}
		return result, nil
	case CreateWorkspaceArgs:
		a.Cwd = expandHome(a.Cwd, home)
		workflowBase := a.Worktree && a.Base == "" && a.Agent != nil &&
			a.Agent.Labels["intelligence.run"] != "" && a.Agent.Labels["intelligence.role"] == "orchestrator"
		if workflowBase {
			a.Base = refreshWorkflowBase(ctx, a.Cwd)
		}
		fallback := workflowBase && a.Base == ""
		request := paseo.CreateWorkspace{Cwd: a.Cwd, Worktree: a.Worktree, Branch: a.Branch, Base: a.Base,
			WorktreeSlug: a.WorktreeSlug, Title: a.Title, IdempotencyKey: key}
		ws, err := c.CreateWorkspace(ctx, request)
		if workflowBase && a.Base != "" && errors.Is(err, paseo.ErrWorkspaceCreationKeyConflict) {
			// An earlier delivery may have fallen back. Replay that exact
			// request under the same key, including after an unreported result.
			request.Base = ""
			ws, err = c.CreateWorkspace(ctx, request)
			fallback = !errors.Is(err, paseo.ErrWorkspaceCreationKeyConflict)
		}
		if workflowBase && errors.Is(err, paseo.ErrWorkspaceCreationKeyConflict) {
			// An earlier delivery may have made the workspace with a different base
			// if origin's default or its availability changed. Resume that creation.
			fallback = false
			ws, err = c.WorkspaceCreation(ctx, key)
		}
		if err != nil || a.Agent == nil {
			return CommandResult{WorkspaceID: ws.ID, WorktreeBaseFallback: fallback}, err
		}
		ag := *a.Agent
		ag.WorkspaceID, ag.Cwd = ws.ID, ws.Directory
		ag.Model, ag.Thinking = w.startModel(ag.Provider, ag.Model, ag.Thinking)
		// its own idempotency key: a repeat returns the same agent, never the workspace
		res, err := createAgent(ctx, c, ag, key, key+":agent", key+":agent:retry:"+cmd.ID)
		res.WorkspaceID = ws.ID
		res.WorktreeBaseFallback = fallback
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
		a.Model, a.Thinking = w.startModel(a.Provider, a.Model, a.Thinking)
		return createAgent(ctx, c, a, key, key, key+":retry:"+cmd.ID)
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
		case WatchAgentKind:
			return r, c.SetAgentLabels(ctx, a.AgentID, map[string]string{LiveLabel: a.Live})
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
func createAgent(ctx context.Context, c *paseo.Client, a CreateAgentArgs, cmdID, key, freshKey string) (CommandResult, error) {
	if err := materializeImages(a.Cwd, a.Attachments); err != nil {
		return CommandResult{}, err
	}
	labels := map[string]string{"intelligence.command": cmdID}
	for k, v := range a.Labels {
		labels[k] = v
	}
	req := paseo.CreateAgent{WorkspaceID: a.WorkspaceID, Cwd: a.Cwd, Provider: a.Provider, Model: a.Model,
		Thinking: a.Thinking, Mode: a.Mode, ServiceTier: a.ServiceTier, Title: a.Title, Prompt: a.Prompt, Labels: labels, IdempotencyKey: key}
	ag, err := c.CreateAgent(ctx, req)
	if err != nil && strings.Contains(err.Error(), keyConflict) {
		// A start retry after a lost connection rebuilds its request (an orchestrator's prompt lists
		// the live runs), and Paseo refuses a different request under the key it already saw. If that
		// earlier request made an agent, attach to it; if Paseo refused the earlier request and no
		// agent carries this command's label, retry with a fresh idempotency key.
		made, ok, listErr := agentMadeBy(ctx, c, cmdID, a.WorkspaceID)
		if ok {
			return CommandResult{AgentID: made.ID, WorkspaceID: made.WorkspaceID}, nil
		}
		if listErr == nil && freshKey != "" && freshKey != key {
			req.IdempotencyKey = freshKey
			ag, err = c.CreateAgent(ctx, req)
		}
	}
	return CommandResult{AgentID: ag.ID, WorkspaceID: ag.WorkspaceID}, err
}

// keyConflict is Paseo's refusal of a creation key reused with a different request.
const keyConflict = "agent_request_key_conflict"

// agentMadeBy finds the live agent a command made, by the label createAgent gives it, in workspace
// (any, if ""). Returns a non-nil error if the daemon could not list agents.
func agentMadeBy(ctx context.Context, c *paseo.Client, cmdID, workspace string) (paseo.Agent, bool, error) {
	agents, err := c.Agents(ctx)
	if err != nil {
		return paseo.Agent{}, false, err
	}
	for _, ag := range agents {
		if ag.Labels["intelligence.command"] == cmdID && (workspace == "" || ag.WorkspaceID == workspace) {
			return ag, true, nil
		}
	}
	return paseo.Agent{}, false, nil
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
