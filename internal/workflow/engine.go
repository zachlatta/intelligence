package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Engine keeps workflows and their runs in Postgres, and moves runs: it queues the commands that
// make each run's project finder, worktree and orchestrator, follows their results, and records
// the orchestrator's moves. It writes as the tables' owner; accounts read through their own roles.
type Engine struct {
	p   *pgxpool.Pool
	reg *fleet.Registry
	Now func() time.Time
	// Limits are when a host is too busy to start another run: a new run queues (queue.go).
	Limits Limits
}

// NewEngine keeps workflows in p's database.
func NewEngine(p *pgxpool.Pool, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{p: p, reg: fleet.New(p), Now: now, Limits: DefaultLimits()}
}

// ErrNotFound: no such workflow or run in the account (or for the host).
var ErrNotFound = errors.New("not found")

// Refusal is a request that cannot be done as asked. Conflict: not in the run's present state
// (else the request itself is wrong). Msg is meant for whoever asked, an agent or a person.
type Refusal struct {
	Conflict bool
	Msg      string
}

func (r *Refusal) Error() string { return r.Msg }

func bad(format string, a ...any) error { return &Refusal{Msg: fmt.Sprintf(format, a...)} }
func conflict(format string, a ...any) error {
	return &Refusal{Conflict: true, Msg: fmt.Sprintf(format, a...)}
}

// Run statuses.
const (
	Queued               = "queued" // waits for its host to have room (queue_reason says why; queue.go)
	FindingProject       = "finding_project"
	StartingOrchestrator = "starting"
	Running              = "running"
	Waiting              = "waiting"
	Stalled              = "stalled"
	Succeeded            = "succeeded"
	Failed               = "failed"
	Canceled             = "canceled"
)

// commandTTL is how long a run's own commands may wait for their host; a reply waits longer, as
// the host may be away when Zach answers.
const (
	commandTTL = time.Hour
	replyTTL   = 24 * time.Hour
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", bad("a workflow needs a name, up to 100 characters")
	}
	return name, nil
}

// WorkflowState is the complete editable state captured for review and undo.
// Version is the monotonic definition revision; deletion preserves it, restores increment it.
type WorkflowState struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Definition  Definition `json:"definition"`
	Version     int        `json:"version"`
	IsDefault   bool       `json:"isDefault"`
	Deleted     bool       `json:"deleted"`
}

func workflowState(ctx context.Context, tx pgx.Tx, account int64, id string) (*WorkflowState, error) {
	var s WorkflowState
	var def []byte
	err := tx.QueryRow(ctx, `SELECT w.name,w.description,w.version,w.is_default,w.deleted_at IS NOT NULL,v.definition
 FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version
 WHERE w.id=$1 AND w.account_id=$2 FOR UPDATE OF w`, id, account).Scan(&s.Name, &s.Description, &s.Version, &s.IsDefault, &s.Deleted, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(def, &s.Definition)
	return &s, err
}

// CreateWorkflow saves a new workflow for account, as version 1.
func (e *Engine) CreateWorkflow(ctx context.Context, account int64, name, description string, d Definition) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		var err error
		id, _, err = e.applyWorkflowState(ctx, tx, account, "", nil, &WorkflowState{Name: name, Description: description, Definition: d})
		return err
	})
	return id, err
}

// SetDefault chooses (or clears) the task workflow shown first for this account.
// Lock the account so concurrent choices replace each other without racing the unique index.
func (e *Engine) SetDefault(ctx context.Context, account int64, id string, chosen bool) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var accountID int64
		if err := tx.QueryRow(ctx, "SELECT id FROM accounts WHERE id = $1 FOR NO KEY UPDATE", account).Scan(&accountID); err != nil {
			return err
		}
		var trigger string
		err := tx.QueryRow(ctx, `SELECT v.definition ->> 'trigger' FROM workflows w
			JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL FOR UPDATE OF w`, id, account).Scan(&trigger)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if chosen && trigger != TriggerTask {
			return bad("only a task workflow can be the default")
		}
		if chosen {
			if _, err := tx.Exec(ctx, "UPDATE workflows SET is_default = false WHERE account_id = $1 AND is_default", account); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE workflows SET is_default = $3 WHERE id = $1 AND account_id = $2", id, account, chosen)
		return err
	})
}

// UpdateWorkflow saves d as the next version of account's workflow id, and returns that version.
// Runs keep the version they started with.
func (e *Engine) UpdateWorkflow(ctx context.Context, account int64, id, name, description string, d Definition) (int, error) {
	if !uuidRe.MatchString(id) {
		return 0, ErrNotFound
	}
	var version int
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		current, err := workflowState(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if current.Deleted {
			return ErrNotFound
		}
		target := &WorkflowState{Name: name, Description: description, Definition: d, IsDefault: current.IsDefault && d.Trigger == TriggerTask}
		_, applied, err := e.applyWorkflowState(ctx, tx, account, id, current, target)
		if err == nil {
			version = applied.Version
		}
		return err
	})
	return version, err
}

// DeleteWorkflow deletes account's workflow id. Its row and versions stay, marked deleted_at, so
// its runs and what happened in them stay readable; it is no longer listed, edited, the default or
// run. A workflow with a run still going is refused: cancel or finish the run first.
func (e *Engine) DeleteWorkflow(ctx context.Context, account int64, id string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		current, err := workflowState(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if current.Deleted {
			return ErrNotFound
		}
		target := *current
		target.Deleted = true
		_, _, err = e.applyWorkflowState(ctx, tx, account, id, current, &target)
		return err
	})
}

// Lock the account before workflows/proposals, including email/default validation.
func lockWorkflowAccount(ctx context.Context, tx pgx.Tx, account int64) error {
	_, err := tx.Exec(ctx, "SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE", account)
	return err
}

// applyWorkflowState is the sole workflow persistence path for direct writes and
// reviewed approval/undo. The caller holds the account and current workflow locks.
// Deletion preserves the definition version; creates, updates and restores save a
// new immutable definition. All deletion paths use terminal status, not timestamps.
func (e *Engine) applyWorkflowState(ctx context.Context, tx pgx.Tx, account int64, id string, current, target *WorkflowState) (string, *WorkflowState, error) {
	s := *target
	at := e.Now()
	if s.Deleted {
		if current == nil || current.Deleted {
			return "", nil, ErrNotFound
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE workflow_id=$1 AND status NOT IN ($2,$3,$4)`, id, Succeeded, Failed, Canceled).Scan(&live); err != nil {
			return "", nil, err
		}
		if live > 0 {
			return "", nil, conflict("this workflow has runs still going (%d): cancel them or let them finish first", live)
		}
		s = *current
		s.Deleted = true
		s.IsDefault = false
		_, err := tx.Exec(ctx, "UPDATE workflows SET deleted_at=$2,is_default=false,updated_at=$2 WHERE id=$1", id, at)
		return id, &s, err
	}
	name, err := checkName(s.Name)
	if err != nil {
		return "", nil, err
	}
	s.Name = name
	s.Description = strings.TrimSpace(s.Description)
	s.Definition = s.Definition.normalized()
	if err := s.Definition.Validate(); err != nil {
		return "", nil, bad("%s", err)
	}
	if err := validateModelTokens(ctx, tx, account, s.Definition); err != nil {
		return "", nil, err
	}
	if err := ownsHost(ctx, tx, account, s.Definition.Host); err != nil {
		return "", nil, err
	}
	if err := uniqueEmailTrigger(ctx, tx, account, id, s.Definition); err != nil {
		return "", nil, err
	}
	if s.IsDefault {
		if s.Definition.Trigger != TriggerTask {
			return "", nil, bad("only a task workflow can be the default")
		}
		var other bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM workflows WHERE account_id=$1 AND is_default AND id::text<>$2)", account, id).Scan(&other); err != nil {
			return "", nil, err
		}
		if other {
			return "", nil, conflict("another workflow is now the default; cannot restore the previous default")
		}
	}
	s.Version = 1
	if current != nil {
		s.Version = current.Version + 1
	}
	if current == nil {
		err = tx.QueryRow(ctx, `INSERT INTO workflows(account_id,name,description,version,created_at,updated_at) VALUES($1,$2,$3,1,$4,$4) RETURNING id::text`, account, s.Name, s.Description, at).Scan(&id)
	} else {
		_, err = tx.Exec(ctx, `UPDATE workflows SET name=$2,description=$3,version=$4,is_default=$5,deleted_at=NULL,updated_at=$6 WHERE id=$1`, id, s.Name, s.Description, s.Version, s.IsDefault, at)
	}
	if err != nil {
		return "", nil, err
	}
	def, _ := json.Marshal(s.Definition)
	_, err = tx.Exec(ctx, "INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,$2,$3,$4,$5)", id, s.Version, account, def, at)
	return id, &s, err
}

// uniqueEmailTrigger refuses an email trigger that another of account's workflows (not id) has
// already, with nothing to tell the two apart: one email starts one run.
func uniqueEmailTrigger(ctx context.Context, tx pgx.Tx, account int64, id string, d Definition) error {
	if d.Trigger != TriggerEmail || d.Email == nil {
		return nil
	}
	others, err := EmailWorkflows(ctx, tx, account)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.ID == id || !d.Email.Overlaps(*o.Def.Email) {
			continue
		}
		if shared := d.Email.Shared(*o.Def.Email); len(shared) > 0 {
			return bad("email from %s already starts “%s”. Remove it from one of them, or add a field (subject, words, To) to tell them apart",
				strings.Join(shared, ", "), o.Name)
		}
		return bad("“%s” already starts for the same email. Add a field (sender, subject, words, To) to tell them apart", o.Name)
	}
	return nil
}

// EmailWorkflow is a workflow whose latest version an email starts.
type EmailWorkflow struct {
	ID, Name string
	Def      Definition
}

// EmailWorkflows are account's workflows whose trigger is email, oldest first.
func EmailWorkflows(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, account int64) ([]EmailWorkflow, error) {
	rows, err := q.Query(ctx, `SELECT w.id::text, w.name, v.definition FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE w.account_id = $1 AND w.deleted_at IS NULL AND v.definition ->> 'trigger' = 'email' ORDER BY w.created_at, w.id`, account)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EmailWorkflow, error) {
		var w EmailWorkflow
		var def []byte
		err := row.Scan(&w.ID, &w.Name, &def)
		if err == nil {
			err = json.Unmarshal(def, &w.Def)
		}
		return w, err
	})
	out := list[:0]
	for _, w := range list {
		if w.Def.Email != nil {
			out = append(out, w)
		}
	}
	return out, err
}

// ownsHost refuses a host that is not account's.
func ownsHost(ctx context.Context, tx pgx.Tx, account int64, host string) error {
	var owns bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM host_keys WHERE host = $1 AND account_id = $2)", host, account).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return bad("host %s is not one of this account's hosts", host)
	}
	return nil
}

// run is a run with its workflow's name and definition.
type run struct {
	ID, Host, Title, Input, Status, Repo, Branch string
	Account                                      int64
	Step                                         int
	WorkspaceID, ProjectAgent, Orchestrator      string
	ProjectCommand                               *string
	StartRetryAt                                 *time.Time
	StartRetries                                 int
	EstimatedMemory                              int64
	ProjectSelected                              bool
	StartedByHand                                bool // Start now: it bypasses its host's queue until it has started
	Name                                         string
	Def                                          Definition
}

func (r run) vars() Vars { return Vars{Run: r.ID, Input: r.Input, Repo: r.Repo, Branch: r.Branch} }

const runSelect = `SELECT r.id::text, r.host, r.title, r.input, r.status, r.repo, r.branch, r.account_id, r.step, r.workspace_id,
	r.project_agent_id, r.orchestrator_id, r.project_command::text, w.name, v.definition, r.start_retry_at, r.start_retries,
	r.estimated_memory_bytes, r.project_selected, r.started_by_hand
	FROM workflow_runs r JOIN workflow_versions v USING (workflow_id, version) JOIN workflows w ON w.id = r.workflow_id `

func scanRun(row pgx.Row) (run, error) {
	var r run
	var def []byte
	err := row.Scan(&r.ID, &r.Host, &r.Title, &r.Input, &r.Status, &r.Repo, &r.Branch, &r.Account, &r.Step, &r.WorkspaceID,
		&r.ProjectAgent, &r.Orchestrator, &r.ProjectCommand, &r.Name, &def, &r.StartRetryAt, &r.StartRetries, &r.EstimatedMemory, &r.ProjectSelected, &r.StartedByHand)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(def, &r.Def)
	}
	return r, err
}

func (e *Engine) event(ctx context.Context, tx pgx.Tx, r run, kind string, step, to *int, text string) error {
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, to_step, text)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, r.ID, r.Account, e.Now(), kind, step, to, text)
	return err
}

func ptr(i int) *int { return &i }

// StartRun starts account's workflow with input, on the host its definition names, and returns
// the run's id. A matched project starts its finder agent; a fixed one, the orchestrator.
func (e *Engine) StartRun(ctx context.Context, account int64, workflowID, input string) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var err error
		id, err = e.StartRunTx(ctx, tx, account, workflowID, input, time.Time{})
		return err
	})
	return id, err
}

// StartRunTx is StartRun in tx, for a request sent at requestedAt (zero: now).
func (e *Engine) StartRunTx(ctx context.Context, tx pgx.Tx, account int64, workflowID, input string, requestedAt time.Time) (string, error) {
	if requestedAt.IsZero() {
		requestedAt = e.Now()
	}
	if strings.TrimSpace(input) == "" {
		return "", bad("a run needs input: what should be done")
	}
	if !uuidRe.MatchString(workflowID) {
		return "", ErrNotFound
	}
	var id, host string
	err := func() error {
		var version int
		var def []byte
		err := tx.QueryRow(ctx, `SELECT w.version, v.definition FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL FOR SHARE OF w`, workflowID, account).Scan(&version, &def)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var d Definition
		if err := json.Unmarshal(def, &d); err != nil {
			return err
		}
		d = d.normalized()
		host = d.Host
		if host == "" {
			return bad("this workflow does not say which host it runs on: pick one in its editor, and save")
		}
		if err := ownsHost(ctx, tx, account, host); err != nil {
			return err
		}
		if err := lockQueue(ctx, tx, host); err != nil {
			return err
		}
		repo := ""
		selected := d.Project.Mode == ProjectFixed
		if selected {
			repo = d.Project.Directory
		}
		estimate, err := repositoryMemory(ctx, tx, account, host, repo, selected)
		if err != nil {
			return err
		}
		why, err := e.room(ctx, tx, host, "", repo, estimate)
		if err != nil {
			return err
		}
		status := FindingProject
		if why != "" {
			status = Queued
		}
		if err := tx.QueryRow(ctx, `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status, queue_reason, created_at, updated_at,
			repo, project_selected, estimated_memory_bytes, queued_at_creation, requested_at, answer_expected)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10, $11, $12, $13, $14, $15) RETURNING id::text`, account, workflowID, version, host, taskName(input), input,
			status, why, e.Now(), repo, selected, estimate, why != "", requestedAt, d.Trigger == TriggerEmail || d.Trigger == TriggerVoiceMemo).Scan(&id); err != nil { // now, holding the lock: the queue's order is the order decided
			return err
		}
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1", id))
		if err != nil {
			return err
		}
		if err := e.event(ctx, tx, r, "started", nil, nil, input); err != nil {
			return err
		}
		if selected {
			if err := e.recordProject(ctx, tx, r, repo); err != nil {
				return err
			}
		}
		if why != "" {
			return e.event(ctx, tx, r, "queued", nil, nil, why)
		}
		return e.launch(ctx, tx, r)
	}()
	if errors.Is(err, fleet.ErrUnknownHost) {
		return "", bad("host %s has not reported yet", host)
	}
	return id, err
}

// launch starts run r: its project finder, or for a fixed project the orchestrator at once. It
// records when (launched_at) and the command that starts it (launch_command), which reserve the
// run's footprint in its host's projected load until the host has carried it out (queue.go).
func (e *Engine) launch(ctx context.Context, tx pgx.Tx, r run) error {
	if err := e.startFirst(ctx, tx, r); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE workflow_runs SET launched_at = $2, launch_command = pending_command WHERE id = $1", r.ID, e.Now())
	return err
}

// startFirst queues the command that starts run r, its pending_command.
func (e *Engine) startFirst(ctx context.Context, tx pgx.Tx, r run) error {
	if r.ProjectSelected {
		return e.startOrchestrator(ctx, tx, r, r.Repo)
	}
	args, err := projectStartArgs(ctx, tx, r)
	if err != nil {
		return err
	}
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.CreateAgentKind, args, commandTTL, e.Now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_runs SET status = $2, pending_command = $3, project_command = $3, updated_at = $4 WHERE id = $1",
		r.ID, FindingProject, c.ID, e.Now())
	return err
}

// repositories lists host's git repositories, including registered projects without workspaces and workspace roots as fallback.
func repositories(ctx context.Context, tx pgx.Tx, host string) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT DISTINCT directory FROM host_repositories WHERE host = $1 ORDER BY 1", host)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Rebuild starts from the run's unresolved input. Reusing commands.args would both
// replay a stale repository catalog and try to resolve already-delivered images twice.
func projectStartArgs(ctx context.Context, tx pgx.Tx, r run) (json.RawMessage, error) {
	repos, err := repositories(ctx, tx, r.Host)
	if err != nil {
		return nil, err
	}
	p := r.Def.Project
	return json.Marshal(hostclient.CreateAgentArgs{Cwd: "~", Provider: p.Provider, Model: p.Model, Thinking: p.Thinking,
		Mode: agentMode(p.Provider), Title: agentTitle("Find the project", r.Name, r.Title), Prompt: ProjectPrompt(r.ID, r.Def, r.Input, repos),
		Labels: map[string]string{"intelligence.run": r.ID, "intelligence.role": "project"}})
}

func (e *Engine) orchestratorStartArgs(ctx context.Context, tx pgx.Tx, r run) (json.RawMessage, error) {
	d, err := e.fillModels(ctx, tx, r, 0)
	if err != nil {
		return nil, err
	}
	wsTitle, _ := runNames(r.ID, r.Name, r.Title)
	o := r.Def.Orchestrator
	agent := &hostclient.CreateAgentArgs{Provider: o.Provider, Model: o.Model, Thinking: o.Thinking, Mode: agentMode(o.Provider),
		Title: agentTitle("Orchestrator", r.Name, r.Title), Prompt: OrchestratorPrompt(r.Name, d, r.vars()),
		Labels: map[string]string{"intelligence.run": r.ID, "intelligence.role": "orchestrator"}}
	ws := hostclient.CreateWorkspaceArgs{Cwd: "~", Title: wsTitle, Agent: agent}
	if r.Repo != "" {
		ws = hostclient.CreateWorkspaceArgs{Cwd: r.Repo, Worktree: true, Branch: r.Branch, Title: wsTitle, Agent: agent}
	}
	return json.Marshal(ws)
}

// startOrchestrator records r's project (repo, or "" for none) and queues the workspace with the
// orchestrator in it: a worktree of repo on the run's branch, or the home directory.
func (e *Engine) startOrchestrator(ctx context.Context, tx pgx.Tx, r run, repo string) error {
	r.Repo = repo
	// the title again from the input: a run started before titles were the task has its first line
	r.Title = taskName(r.Input)
	wsTitle, branch := runNames(r.ID, r.Name, r.Title)
	if repo != "" {
		r.Branch = branch
	}
	args, err := e.orchestratorStartArgs(ctx, tx, r)
	if err != nil {
		return err
	}
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.CreateWorkspaceKind, args, commandTTL, e.Now())
	if err != nil {
		return err
	}
	if err := e.recordProject(ctx, tx, r, repo); err != nil {
		return err
	}
	// the workspace becomes the command that starts the run (launch_command): a run whose finder ran
	// keeps its place in its host's projected load until the host has made its orchestrator too
	_, err = tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, repo = $3, branch = $4, workspace_title = $5, pending_command = $6, launch_command = $6, updated_at = $7,
		start_retries=0, start_retry_at=NULL, start_retry_queued=false, error='',
		title = $8 WHERE id = $1`, r.ID, StartingOrchestrator, repo, r.Branch, wsTitle, c.ID, e.Now(), r.Title)
	return err
}

// The selection event precedes any capacity wait. Legacy queued selections may
// lack it, so launch also calls this without duplicating a recorded choice.
func (e *Engine) recordProject(ctx context.Context, tx pgx.Tx, r run, repo string) error {
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id,account_id,at,kind,text)
		SELECT $1,$2,$3,'project',$4 WHERE NOT EXISTS
		(SELECT 1 FROM workflow_run_events WHERE run_id=$1 AND kind='project')`, r.ID, r.Account, e.Now(), orNone(repo))
	return err
}

// CommandUpdated follows a host's update to a command a run waits on: the finder or the
// orchestrator started (or not).
func (e *Engine) CommandUpdated(ctx context.Context, commandID string) error {
	c, err := e.reg.Command(ctx, commandID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || !c.Finished() {
		return err
	}
	var res hostclient.CommandResult
	if len(c.Result) > 0 {
		_ = json.Unmarshal(c.Result, &res)
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		// Queue admission and retry eligibility share this lock. Take it
		// before the run row, in the same order as startNext.
		if err := lockQueue(ctx, tx, c.Host); err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.pending_command = $1 FOR UPDATE OF r", commandID))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		at := e.Now()
		if c.Status != hostclient.CommandSucceeded {
			what := "could not start the project finder"
			if r.Status == StartingOrchestrator {
				what = "could not start the orchestrator"
			}
			msg := what + ": " + c.Error
			if res.WorkspaceID == "" {
				res.WorkspaceID = r.WorkspaceID
			}
			if r.Status == FindingProject || r.Status == StartingOrchestrator {
				var handled bool
				if handled, msg, err = e.retryStart(ctx, tx, r, c, msg, res.WorkspaceID, at); handled || err != nil {
					return err
				}
			}
			if err := e.event(ctx, tx, r, "error", nil, nil, msg); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, error = $3, pending_command = NULL, start_retry_at=NULL, start_retry_queued=false, workspace_id = $4,
				updated_at = $5, finished_at = $5 WHERE id = $1`, r.ID, Failed, msg, res.WorkspaceID, at)
			if err != nil {
				return err
			}
			r.WorkspaceID, r.Orchestrator = res.WorkspaceID, res.AgentID
			return e.archiveRun(ctx, tx, r)
		}
		if r.Status == FindingProject {
			_, err := tx.Exec(ctx, "UPDATE workflow_runs SET project_agent_id = $2, pending_command = NULL, error='', start_retries=0, start_retry_at=NULL, start_retry_queued=false, updated_at = $3 WHERE id = $1",
				r.ID, res.AgentID, at)
			return err
		}
		if err := e.event(ctx, tx, r, "workspace", nil, nil, res.AgentID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, step = 0, workspace_id = $3, orchestrator_id = $4,
			pending_command = NULL, error='', start_retry_at=NULL, start_retry_queued=false, updated_at = $5 WHERE id = $1`, r.ID, Running, res.WorkspaceID, res.AgentID, at)
		return err
	})
}

// Move is what `intelligence workflow` asks of a run.
type Move struct {
	Move      string `json:"move"`                // summarize, project, next, skip, back, ask, done, fail
	Directory string `json:"directory,omitempty"` // project: a repository, or "none"
	Step      string `json:"step,omitempty"`      // back: the step's id
	Summary   string `json:"summary,omitempty"`
	Why       string `json:"why,omitempty"`
	Note      string `json:"note,omitempty"`    // ask: Markdown, for Zach
	Token     string `json:"token,omitempty"`   // suggest: a model group's stable token
	Exclude   string `json:"exclude,omitempty"` // suggest: comma-separated provider/model routes to skip
}

// MoveRun makes m on run, for host (whose key the caller checked): the run must be on host. It
// returns what the agent should read next.
func (e *Engine) MoveRun(ctx context.Context, host, runID string, m Move) (string, error) {
	if !uuidRe.MatchString(runID) {
		return "", ErrNotFound
	}
	var msg string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockQueue(ctx, tx, host); err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1 FOR UPDATE OF r", runID))
		if err != nil {
			return err
		}
		if r.Host != host {
			return ErrNotFound
		}
		if r.Status == Stalled {
			// Move validation below shares this transaction: an invalid move rolls
			// back both the recovery event and the state change.
			if err := e.unstall(ctx, tx, r, "Resumed: orchestrator continued with "+m.Move+"."); err != nil {
				return err
			}
			r.Status = Running
		}
		msg, err = e.move(ctx, tx, r, m)
		return err
	})
	return msg, err
}

func (e *Engine) move(ctx context.Context, tx pgx.Tx, r run, m Move) (string, error) {
	// A label is independent of the run's lifecycle, so historical and in-flight runs
	// can be backfilled without restarting agents or advancing a step.
	if m.Move == "summarize" {
		summary := strings.TrimSpace(m.Summary)
		if summary == "" || utf8.RuneCountInString(summary) > 80 || strings.ContainsFunc(m.Summary, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
		}) {
			return "", bad("a task summary must be one line of 1–80 characters")
		}
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET task_summary = $2, updated_at = $3 WHERE id = $1", r.ID, summary, e.Now()); err != nil {
			return "", err
		}
		return "Task summary saved. Only the label changed; run status and step are unchanged.", e.event(ctx, tx, r, "summarize", nil, nil, summary)
	}
	switch r.Status {
	case Canceled:
		return "", conflict("Zach canceled this run. Stop: do nothing more for it.")
	case Succeeded, Failed:
		return "", conflict("This run is over (%s). Do nothing more for it.", r.Status)
	case Queued:
		return "", conflict("This run is queued for room on its host. Do nothing for it.")
	}
	if m.Move == "project" {
		if r.Status != FindingProject {
			return "", conflict("The project is already chosen: %s.", orNone(r.Repo))
		}
		repo := strings.TrimSpace(m.Directory)
		if repo == "none" {
			repo = ""
		} else {
			repos, err := repositories(ctx, tx, r.Host)
			if err != nil {
				return "", err
			}
			if !contains(repos, repo) {
				return "", bad("%q is not one of this host's repositories: %s, or none.", repo, strings.Join(repos, ", "))
			}
		}
		if err := e.archiveFinder(ctx, tx, r); err != nil {
			return "", err
		}
		estimate, err := repositoryMemory(ctx, tx, r.Account, r.Host, repo, true)
		if err != nil {
			return "", err
		}
		r.Repo, r.ProjectSelected, r.EstimatedMemory = repo, true, estimate
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET repo=$2, project_selected=true, estimated_memory_bytes=$3 WHERE id=$1`, r.ID, repo, estimate); err != nil {
			return "", err
		}
		if err := e.recordProject(ctx, tx, r, repo); err != nil {
			return "", err
		}
		// This run already holds a startup reservation. Replace it with the selected
		// project's estimate; do not count the finder twice or lose its FIFO priority.
		// A run started by hand was started ahead of the queue and whatever the load:
		// its orchestrator starts too, rather than waiting behind the queue it skipped.
		why := ""
		if !r.StartedByHand {
			why, err = e.room(ctx, tx, r.Host, r.ID, repo, estimate)
			if err != nil {
				return "", err
			}
		}
		if why != "" {
			if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET status='queued', queue_reason=$2, pending_command=NULL,
				start_retry_at=NULL, start_retry_queued=false, updated_at=$3 WHERE id=$1`, r.ID, why, e.Now()); err != nil {
				return "", err
			}
			return "Project selected. The orchestrator is queued for host capacity. Stop now.", e.event(ctx, tx, r, "queued", nil, nil, why)
		}
		if err := e.startOrchestrator(ctx, tx, r, repo); err != nil {
			return "", err
		}
		return fmt.Sprintf("Thanks: the project is %s. The orchestrator takes it from here. Stop now.", orNone(repo)), nil
	}
	switch r.Status {
	case FindingProject, StartingOrchestrator:
		return "", conflict("This run has no orchestrator yet (%s).", strings.ReplaceAll(r.Status, "_", " "))
	case Waiting:
		return "", conflict("Zach has not replied yet. End your turn: his reply comes as your next message.")
	}
	last := len(r.Def.Steps) - 1
	set := func(status string, step int, note string) error {
		var finished any
		errText := ""
		if status == Succeeded || status == Failed {
			finished = e.Now()
		}
		if status == Failed {
			errText = m.Why
		}
		if finished != nil {
			if err := fleet.CaptureWorkflowAgents(ctx, tx, r.Host, r.ID, e.Now()); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, step = $3, note = $4, error = $5, updated_at = $6,
			finished_at = $7 WHERE id = $1`, r.ID, status, step, note, errText, e.Now(), finished)
		if err != nil {
			return err
		}
		if status == Succeeded {
			return e.archiveRun(ctx, tx, r)
		}
		return nil
	}
	switch m.Move {
	case "next", "skip", "done":
		text := m.Summary
		if m.Move == "skip" {
			text = m.Why
		}
		if m.Move == "done" || r.Step >= last {
			if err := e.event(ctx, tx, r, "done", ptr(r.Step), nil, text); err != nil {
				return "", err
			}
			return "That was the last step: the run is finished. Archive any sub-agents you started outside this workspace, then end your turn. The workspace and its agents will be archived automatically.", set(Succeeded, r.Step, "")
		}
		if err := e.event(ctx, tx, r, m.Move, ptr(r.Step), ptr(r.Step+1), text); err != nil {
			return "", err
		}
		d, err := e.fillModels(ctx, tx, r, r.Step+1)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Step %d is done.\n\n%s", r.Step+1, StepMessage(d, r.Step+1, r.vars())+sameTurnReminder), set(Running, r.Step+1, "")
	case "back":
		to := r.Def.Index(m.Step)
		if to < 0 || to >= r.Step {
			return "", bad("back goes to an earlier step, by id: %s.", strings.Join(stepIDs(r.Def.Steps[:r.Step]), ", "))
		}
		if strings.TrimSpace(m.Why) == "" {
			return "", bad("say why with --why: the step it goes back to needs to know what is missing.")
		}
		if err := e.event(ctx, tx, r, "back", ptr(r.Step), ptr(to), m.Why); err != nil {
			return "", err
		}
		d, err := e.fillModels(ctx, tx, r, to)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Back to step %d. What is missing, as you said: %s\n\n%s", to+1, m.Why, StepMessage(d, to, r.vars())+sameTurnReminder), set(Running, to, "")
	case "ask":
		if strings.TrimSpace(m.Note) == "" {
			return "", bad("ask needs a note for Zach: --note <file>, in Markdown.")
		}
		if err := e.event(ctx, tx, r, "ask", ptr(r.Step), nil, m.Note); err != nil {
			return "", err
		}
		return "Asked Zach. End your turn now: his reply comes as your next message.", set(Waiting, r.Step, m.Note)
	case "fail":
		if strings.TrimSpace(m.Why) == "" {
			return "", bad("say why with --why.")
		}
		if err := e.event(ctx, tx, r, "fail", ptr(r.Step), nil, m.Why); err != nil {
			return "", err
		}
		return "The run is marked failed. The workspace stays for inspection. Archive any sub-agents you started outside this workspace, then end your turn.", set(Failed, r.Step, "")
	}
	return "", bad("unknown move %q: want next, back, skip, ask, done or fail.", m.Move)
}

const sameTurnReminder = "\n\nContinue in this same turn. Wait for sub-agents in the foreground; end your turn only after a successful ask or when the run is finished. Leave every gate to its instructions."

// archiveRun queues durable cleanup in the same transaction as the terminal move. Its
// command's outcome is independent of the run's outcome and readable through archive_command.
func (e *Engine) archiveRun(ctx context.Context, tx pgx.Tx, r run) error {
	if r.WorkspaceID == "" {
		return nil
	}
	args, _ := json.Marshal(hostclient.WorkspaceArgs{WorkspaceID: r.WorkspaceID, WaitForAgent: r.Orchestrator})
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.ArchiveWorkspaceKind, args, replyTTL, e.Now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_runs SET archive_command = $2 WHERE id = $1", r.ID, c.ID)
	return err
}

// archiveFinder queues archiving r's project finder, once its agent is known.
func (e *Engine) archiveFinder(ctx context.Context, tx pgx.Tx, r run) error {
	agent := r.ProjectAgent
	if agent == "" && r.ProjectCommand != nil {
		c, err := e.reg.Command(ctx, *r.ProjectCommand)
		if err == nil && len(c.Result) > 0 {
			var res hostclient.CommandResult
			_ = json.Unmarshal(c.Result, &res)
			agent = res.AgentID
		}
	}
	if agent == "" {
		return nil
	}
	args, _ := json.Marshal(hostclient.AgentArgs{AgentID: agent})
	_, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.ArchiveAgentKind, args, commandTTL, e.Now())
	return err
}

// Reply sends Zach's reply to the note account's run is waiting on, to its orchestrator.
func (e *Engine) Reply(ctx context.Context, account int64, runID, text string) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error { return e.ReplyTx(ctx, tx, account, runID, text) })
}

// ReplyTx is Reply in tx.
func (e *Engine) ReplyTx(ctx context.Context, tx pgx.Tx, account int64, runID, text string) error {
	if strings.TrimSpace(text) == "" {
		return bad("a reply needs text")
	}
	r, err := e.lockRun(ctx, tx, account, runID)
	if err != nil {
		return err
	}
	if r.Status != Waiting && r.Status != Stalled {
		return conflict("this run is not waiting for a reply (%s)", r.Status)
	}
	heading := "Zach replied to your note:"
	if r.Status == Stalled {
		heading = "A reply arrived for this workflow:"
		if err := e.unstall(ctx, tx, r, "Resumed: a reply arrived for the existing orchestrator."); err != nil {
			return err
		}
	}
	args, _ := json.Marshal(hostclient.AgentArgs{AgentID: r.Orchestrator, Text: fmt.Sprintf("%s\n\n%s\n\n"+
		"Decide what it means for the run, then move it with intelligence workflow (next, back, skip, ask or fail) --run %s.", heading, text, r.ID)})
	if _, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.SendMessageKind, args, replyTTL, e.Now()); err != nil {
		return err
	}
	if err := e.event(ctx, tx, r, "reply", ptr(r.Step), nil, text); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_runs SET status = $2, note = '', updated_at = $3 WHERE id = $1", r.ID, Running, e.Now())
	return err
}

// MessageTx gives account's live run more to go on: what arrived later in the thread that started
// it (an email). While it is queued or its project is being found, it joins the run's input, which the orchestrator
// starts with; once the orchestrator runs, it goes to it as a message. It cannot reach an
// orchestrator that is being started, and refuses a run that is over.
func (e *Engine) MessageTx(ctx context.Context, tx pgx.Tx, account int64, runID, what, text string) error {
	r, err := e.lockRun(ctx, tx, account, runID)
	if err != nil {
		return err
	}
	switch r.Status {
	case Succeeded, Failed, Canceled:
		return conflict("the run is over (%s)", r.Status)
	case StartingOrchestrator:
		return conflict("the run's orchestrator was still starting")
	case FindingProject, Queued:
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET input = input || $2, updated_at = $3 WHERE id = $1",
			r.ID, "\n\n"+what+":\n\n"+text, e.Now()); err != nil {
			return err
		}
		return e.event(ctx, tx, r, "message", nil, nil, what)
	}
	if err := e.unstall(ctx, tx, r, "Resumed: new input arrived for the existing orchestrator."); err != nil {
		return err
	}
	waiting := ""
	if r.Status == Waiting {
		waiting = "\n\nYou are still waiting on Zach's reply to your note: this is not it."
	}
	args, _ := json.Marshal(hostclient.AgentArgs{AgentID: r.Orchestrator, Text: fmt.Sprintf("%s:\n\n%s%s\n\n"+
		"Decide what it means for the run, and carry on.", what, text, waiting)})
	if _, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.SendMessageKind, args, replyTTL, e.Now()); err != nil {
		return err
	}
	return e.event(ctx, tx, r, "message", ptr(r.Step), nil, what)
}

// RunStatus is account's run's status and workflow.
func (e *Engine) RunStatus(ctx context.Context, tx pgx.Tx, account int64, runID string) (status, workflowID string, err error) {
	err = tx.QueryRow(ctx, "SELECT status, workflow_id::text FROM workflow_runs WHERE id = $1 AND account_id = $2", runID, account).Scan(&status, &workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return status, workflowID, err
}

func (e *Engine) lockRun(ctx context.Context, tx pgx.Tx, account int64, runID string) (run, error) {
	if !uuidRe.MatchString(runID) {
		return run{}, ErrNotFound
	}
	return scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1 AND r.account_id = $2 FOR UPDATE OF r", runID, account))
}

// Cancel ends account's run and stops its agent (the orchestrator, or the project finder).
func (e *Engine) Cancel(ctx context.Context, account int64, runID string) error {
	return e.forAccount(ctx, account, runID, func(tx pgx.Tx, r run) error {
		switch r.Status {
		case Succeeded, Failed, Canceled:
			return conflict("this run is already over (%s)", r.Status)
		}
		for _, agent := range []string{r.Orchestrator, r.ProjectAgent} {
			if agent != "" {
				args, _ := json.Marshal(hostclient.AgentArgs{AgentID: agent})
				if _, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.CancelAgentKind, args, commandTTL, e.Now()); err != nil {
					return err
				}
				break
			}
		}
		if err := fleet.CaptureWorkflowAgents(ctx, tx, r.Host, r.ID, e.Now()); err != nil {
			return err
		}
		if err := e.event(ctx, tx, r, "cancel", nil, nil, ""); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, pending_command = NULL, start_retry_at=NULL, start_retry_queued=false, updated_at = $3, finished_at = $3 WHERE id = $1`,
			r.ID, Canceled, e.Now())
		return err
	})
}

func (e *Engine) forAccount(ctx context.Context, account int64, runID string, f func(pgx.Tx, run) error) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1 AND r.account_id = $2 FOR UPDATE OF r", runID, account))
		if err != nil {
			return err
		}
		return f(tx, r)
	})
}

func orNone(repo string) string {
	if repo == "" {
		return "none (the home directory)"
	}
	return repo
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stepIDs(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}
