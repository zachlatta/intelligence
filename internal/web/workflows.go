package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Workflows. Writes go through the JSON API below; pages read what they show through the
// account's own role, like everything else. A host's agents move runs with the host's key
// (`intelligence workflow`, POST /api/workflow-runs/{id}/moves).

//go:embed workflows.html
var workflowsHTML string

var workflowsTmpl = template.Must(template.New("workflows").Funcs(template.FuncMap{
	"memoryGiB": func(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) },
	"short":     short, "ago": ago, "markdown": markdown, "plural": plural, "emailSummary": emailSummary, "voiceMemoSummary": voiceMemoSummary,
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		return template.JS(b), err //nolint:gosec // JSON from the server's own values, in a script
	},
	"add": func(a, b int) int { return a + b },
	"kind": func(k string) string {
		return map[string]string{"step": "Step", "gate": "Gate", "signoff": "Sign-off"}[k]
	},
}).Parse(workflowsHTML + layoutHTML + modelGroupsHTML))

const maxWorkflow = 1 << 20

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkflow))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// workflowError answers err from the engine: 404, 400 or 409 for what the caller can fix.
func workflowError(w http.ResponseWriter, err error) {
	var ref *workflow.Refusal
	var badCommand *fleet.BadCommandError
	switch {
	case errors.As(err, &badCommand):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, workflow.ErrNotFound):
		http.Error(w, "no such workflow or run in this account", http.StatusNotFound)
	case errors.As(err, &ref) && ref.Conflict:
		http.Error(w, ref.Msg, http.StatusConflict)
	case errors.As(err, &ref):
		http.Error(w, ref.Msg, http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type workflowRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  json.RawMessage `json:"definition"`
}

func (req workflowRequest) parse(w http.ResponseWriter) (workflow.Definition, bool) {
	d, err := workflow.Parse(req.Definition)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return d, false
	}
	return d, true
}

// createWorkflow: POST /api/workflows {"name", "description", "definition"}.
func (a *app) createWorkflow(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflowRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		d, ok := req.parse(w)
		if !ok {
			return
		}
		id, err := a.wf.CreateWorkflow(r.Context(), v.acct.ID, req.Name, req.Description, d)
		if err != nil {
			workflowError(w, err)
			return
		}
		w.Header().Set("Location", "/workflows/"+id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "version": 1})
	}
}

// updateWorkflow: PUT /api/workflows/{id}, the same body: a new version.
func (a *app) updateWorkflow(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflowRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		d, ok := req.parse(w)
		if !ok {
			return
		}
		id := r.PathValue("id")
		version, err := a.wf.UpdateWorkflow(r.Context(), v.acct.ID, id, req.Name, req.Description, d)
		if err != nil {
			workflowError(w, err)
			return
		}
		writeJSON(w, map[string]any{"id": id, "version": version})
	}
}

// setDefaultWorkflow: PUT /api/workflows/{id}/default {"default": true|false}.
func (a *app) setDefaultWorkflow(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Default *bool `json:"default"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Default == nil {
			http.Error(w, "default is required", http.StatusBadRequest)
			return
		}
		if err := a.wf.SetDefault(r.Context(), v.acct.ID, r.PathValue("id"), *req.Default); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteWorkflow: DELETE /api/workflows/{id}: 204; 409 while a run of it is still going. Its runs stay.
func (a *app) deleteWorkflow(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.wf.DeleteWorkflow(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// startRun: POST /api/workflow-runs {"workflowId", "input"}: on the host the workflow names.
func (a *app) startRun(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkflowID string `json:"workflowId"`
			Input      string `json:"input"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := a.wf.StartRun(r.Context(), v.acct.ID, req.WorkflowID, req.Input)
		if err != nil {
			workflowError(w, err)
			return
		}
		w.Header().Set("Location", "/workflow-runs/"+id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

// replyToRun: POST /api/workflow-runs/{id}/reply {"text"}: Zach's answer to the note the run waits on.
func (a *app) replyToRun(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := a.wf.Reply(r.Context(), v.acct.ID, r.PathValue("id"), req.Text); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// cancelRun: POST /api/workflow-runs/{id}/cancel {}.
func (a *app) cancelRun(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.wf.Cancel(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			workflowError(w, err)
			return
		}
		a.startQueued(r.Context()) // a run that ended may make room for a queued one
		w.WriteHeader(http.StatusNoContent)
	}
}

// startQueuedRun: POST /api/workflow-runs/{id}/start {}: a queued run starts now, ahead of its
// queue, whatever its host's load.
func (a *app) startQueuedRun(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.wf.StartNow(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// startQueued starts the queued runs whose hosts have room. It is best effort: what it misses,
// the queue's own ticker picks up.
func (a *app) startQueued(ctx context.Context) {
	if err := a.wf.StartQueued(ctx); err != nil {
		log.Printf("workflow queue: %v", err)
	}
}

// moveRun: POST /api/workflow-runs/{id}/moves, from a host's agent with the host's key
// (`intelligence workflow`). It answers {"message"}: what the agent should read next.
func (a *app) moveRun(w http.ResponseWriter, r *http.Request) {
	_, host, ok := a.hostCaller(w, r)
	if !ok {
		return
	}
	var m workflow.Move
	if !decodeWorkflowJSON(w, r, &m) {
		return
	}
	msg, err := a.wf.MoveRun(r.Context(), host, r.PathValue("id"), m)
	if err == nil && m.Move != "summarize" { // labeling a run must not start it or another queued run
		a.startQueued(r.Context()) // a run that ended may make room for a queued one
	}
	workflowResult(w, msg, err)
}

func decodeWorkflowJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkflow))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		workflowMessage(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	return true
}

func workflowMessage(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func workflowResult(w http.ResponseWriter, msg string, err error) {
	if err != nil {
		var ref *workflow.Refusal
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			code, msg = http.StatusNotFound, "no such run on this host"
		case errors.As(err, &ref) && ref.Conflict:
			code, msg = http.StatusConflict, ref.Msg
		case errors.As(err, &ref):
			code, msg = http.StatusBadRequest, ref.Msg
		default:
			log.Printf("workflow host request: %v", err)
			msg = "Could not complete the workflow request. Please try again."
		}
		workflowMessage(w, code, msg)
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// --- reads, through the account's role ---

type workflowRow struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	Version          int                 `json:"version"`
	IsDefault        bool                `json:"isDefault"`
	Definition       workflow.Definition `json:"definition"`
	PendingProposals int                 `json:"pendingProposals"`
}

func readWorkflows(ctx context.Context, p *pgxpool.Pool, id string) ([]workflowRow, error) {
	rows, err := p.Query(ctx, `SELECT w.id::text, w.name, w.description, w.version, w.is_default, v.definition,
		(SELECT count(*) FROM workflow_proposal_status p WHERE p.workflow_id=w.id AND p.effective_status='pending') FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE w.deleted_at IS NULL AND ($1 = '' OR w.id::text = $1) ORDER BY w.is_default DESC, w.created_at, w.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (workflowRow, error) {
		var w workflowRow
		var def []byte
		err := row.Scan(&w.ID, &w.Name, &w.Description, &w.Version, &w.IsDefault, &def, &w.PendingProposals)
		if err == nil {
			err = json.Unmarshal(def, &w.Definition)
		}
		return w, err
	})
}

// runRow is a run as the pages show it.
type runRow struct {
	ID, Title, Input, Status, Host, Repo, Branch, Workflow, WorkflowID   string
	WorkspaceID, WorkspaceTitle, Orchestrator, ProjectAgent, Note, Error string
	QueueReason                                                          string
	Position                                                             int // in its host's queue (workflow_run_queue), 1 next; 0 not queued
	EstimatedMemory                                                      int64
	ProjectSelected, QueuedAtCreation                                    bool
	Step, Version                                                        int
	WorkflowDeleted                                                      bool
	Steps                                                                []workflow.Step
	CreatedAt, UpdatedAt                                                 time.Time
	FinishedAt                                                           *time.Time
	StartRetries                                                         int
	StartRetryAt                                                         *time.Time
	HostOnline, DaemonConnected, RetryKeys                               bool
}

const runRowSelect = `SELECT r.id::text, coalesce(nullif(r.task_summary, ''), r.title), r.input, r.status, r.host, r.repo, r.branch, w.name, r.workflow_id::text, r.workspace_id, r.workspace_title,
	r.orchestrator_id, r.project_agent_id, r.note, r.error, r.step, r.version, v.definition -> 'steps', r.created_at, r.updated_at, r.finished_at,
	w.deleted_at IS NOT NULL, r.start_retries, r.start_retry_at,
	COALESCE((SELECT online FROM host_status WHERE name=r.host),false),
	COALESCE((SELECT online AND connected FROM host_status WHERE name=r.host),false),
	COALESCE((SELECT start_retry_keys FROM hosts WHERE name=r.host),false), r.queue_reason, coalesce(q.position, 0), r.estimated_memory_bytes,
	r.project_selected, r.queued_at_creation
	FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version
	LEFT JOIN workflow_run_queue q ON q.run_id = r.id `

func scanRunRow(row pgx.CollectableRow) (runRow, error) {
	var r runRow
	var steps []byte
	err := row.Scan(&r.ID, &r.Title, &r.Input, &r.Status, &r.Host, &r.Repo, &r.Branch, &r.Workflow, &r.WorkflowID, &r.WorkspaceID, &r.WorkspaceTitle,
		&r.Orchestrator, &r.ProjectAgent, &r.Note, &r.Error, &r.Step, &r.Version, &steps, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt, &r.WorkflowDeleted,
		&r.StartRetries, &r.StartRetryAt, &r.HostOnline, &r.DaemonConnected, &r.RetryKeys, &r.QueueReason, &r.Position, &r.EstimatedMemory, &r.ProjectSelected, &r.QueuedAtCreation)
	if err == nil {
		err = json.Unmarshal(steps, &r.Steps)
	}
	return r, err
}

// Where reads as: what a run is doing now.
func (r runRow) Where() string {
	if r.Position > 0 {
		return r.InLine()
	}
	switch r.Status {
	case workflow.Queued:
		return r.InLine()
	case workflow.FindingProject:
		return "finding the project"
	case workflow.StartingOrchestrator:
		return "starting the orchestrator"
	case workflow.Running, workflow.Waiting, workflow.Stalled:
		if r.Step < len(r.Steps) {
			return r.Steps[r.Step].Title
		}
	}
	return ""
}

// InLine says where a queued run is in its host's queue: "2nd in line for host-1".
func (r runRow) InLine() string {
	if r.Position < 1 {
		return "queued for room on " + r.Host
	}
	return ordinal(r.Position) + " in line for " + r.Host
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// StatusLabel and StatusClass show a run's status as the pages' badges do.
func (r runRow) StatusLabel() string {
	if r.RetryStatus() != "" {
		if r.StartRetryAt != nil {
			return "retry queued"
		}
		return "retrying"
	}
	switch r.Status {
	case workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running:
		return "running"
	case workflow.Stalled:
		return "Stalled"
	case workflow.Waiting:
		return "needs you"
	}
	return r.Status
}

// RetryStatus describes either the next queued retry or the delivery in progress.
func (r runRow) RetryStatus() string {
	if r.Error == "" || (r.Status != workflow.FindingProject && r.Status != workflow.StartingOrchestrator) {
		return ""
	}
	if r.StartRetryAt != nil {
		return fmt.Sprintf("Retry %d of %d queued", r.StartRetries+1, workflow.StartRetryLimit)
	}
	return fmt.Sprintf("Retry %d of %d sent", r.StartRetries, workflow.StartRetryLimit)
}

func (r runRow) RetryDeadline() time.Time {
	if r.StartRetryAt == nil {
		return time.Time{}
	}
	return r.StartRetryAt.Add(workflow.RetryWaitLimit)
}

func (r runRow) StatusClass() string {
	switch r.Status {
	case workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running:
		return "st-running"
	case workflow.Waiting:
		return "st-needs"
	case workflow.Queued:
		return "st-queued"
	case workflow.Succeeded:
		return "st-done"
	case workflow.Failed, workflow.Stalled:
		return "st-error"
	}
	return ""
}

func (r runRow) Live() bool {
	return r.FinishedAt == nil
}

type runEvent struct {
	Kind         string
	Step, ToStep *int
	Text         string
	At           time.Time
}

// stepView is one step of a run as its page shows it.
type stepView struct {
	workflow.Step
	N      int    // 1-based
	State  string // pending, running, asking, done, skipped, sent-back
	Label  string
	Events []runEvent
	Visits []time.Duration // how long each time the orchestrator was handed it took, the current one so far
	Spent  time.Duration   // all of them
	OnIt   bool            // the run is on it now
}

// stepVisit is one time a run was on a step (workflow_run_step_visits); End is nil while it is on it.
type stepVisit struct {
	Step  int
	Start time.Time
	End   *time.Time
}

// SpentLabel is the time spent on the step, as its node shows it; "" for a step not yet reached.
func (s stepView) SpentLabel() string {
	if len(s.Visits) == 0 {
		return ""
	}
	if s.OnIt {
		return humanDuration(s.Spent) + " so far"
	}
	return humanDuration(s.Spent)
}

// SpentDetail is the time on each visit, for the label's tooltip.
func (s stepView) SpentDetail() string {
	if len(s.Visits) < 2 {
		if s.OnIt {
			return "so far"
		}
		return "1 visit"
	}
	parts := make([]string, len(s.Visits))
	for i, d := range s.Visits {
		parts[i] = humanDuration(d)
	}
	if s.OnIt {
		parts[len(parts)-1] += " so far"
	}
	return fmt.Sprintf("%d visits: %s", len(s.Visits), strings.Join(parts, ", "))
}

func stepViews(r runRow, events []runEvent, visits []stepVisit, now time.Time) []stepView {
	out := make([]stepView, len(r.Steps))
	for i, s := range r.Steps {
		out[i] = stepView{Step: s, N: i + 1, State: "pending", Label: "waiting"}
	}
	for _, e := range events {
		if e.Step == nil || *e.Step >= len(out) {
			continue
		}
		v := &out[*e.Step]
		v.Events = append(v.Events, e)
		switch e.Kind {
		case "next", "done":
			v.State, v.Label = "done", "done"
		case "skip":
			v.State, v.Label = "skipped", "skipped"
		case "back":
			v.State, v.Label = "sent-back", "sent back"
		case "ask":
			v.State, v.Label = "asking", "asked you"
		case "reply":
			v.State, v.Label = "done", "replied"
		case "fail":
			v.State, v.Label = "failed", "failed"
		}
	}
	if (r.Status == workflow.Running || r.Status == workflow.Waiting || r.Status == workflow.Stalled) && r.Step >= 0 && r.Step < len(out) {
		v := &out[r.Step]
		v.State, v.Label = "running", "running"
		if r.Status == workflow.Stalled {
			v.State, v.Label = "stalled", "stalled"
		}
		if r.Status == workflow.Waiting {
			v.State, v.Label = "asking", "asking you"
		}
	}
	for _, sv := range visits {
		if sv.Step < 0 || sv.Step >= len(out) {
			continue
		}
		v := &out[sv.Step]
		end := now
		if sv.End != nil {
			end = *sv.End
		} else {
			v.OnIt = true
		}
		d := max(0, end.Sub(sv.Start))
		v.Visits = append(v.Visits, d)
		v.Spent += d
	}
	return out
}

type agentRow struct {
	ID, Title, Provider, Model, Status, Parent string
	Depth                                      int
}

// runAgents are the run's project finder and orchestrator on its host, and the orchestrator's
// sub-agents, theirs, and so on, as a tree in order.
func runAgents(ctx context.Context, p *pgxpool.Pool, r runRow) ([]agentRow, error) {
	roots := []string{}
	for _, id := range []string{r.ProjectAgent, r.Orchestrator} {
		if id != "" {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	// path orders the tree: each agent under its parent, siblings by when they started
	rows, err := p.Query(ctx, `WITH RECURSIVE members AS (
        SELECT a.* FROM recorded_agents a WHERE a.host=$1 AND ($3::boolean OR
            (NOT EXISTS (SELECT 1 FROM workflow_run_agents h WHERE h.run_id=$4)
                AND EXISTS (SELECT 1 FROM agents live WHERE live.host=a.host AND live.id=a.id)
                AND (a.created_at IS NULL OR a.created_at <= $5::timestamptz OR a.id=ANY($2))) OR EXISTS
            (SELECT 1 FROM workflow_run_agents h WHERE h.run_id=$4 AND h.host=a.host AND h.agent_id=a.id))
    ), tree AS (
		SELECT a.id, a.title, a.provider, a.model, a.status, a.parent_id, 0 AS depth,
			ARRAY[lpad(array_position($2::text[], a.id)::text, 3, '0')] AS path
		FROM members a WHERE a.host = $1 AND a.id = ANY($2)
		UNION ALL
		SELECT a.id, a.title, a.provider, a.model, a.status, a.parent_id, t.depth + 1,
			t.path || (coalesce(to_char(a.created_at AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS'), '') || a.id)
		FROM members a JOIN tree t ON a.parent_id = t.id WHERE a.host = $1 AND t.depth < 6)
		SELECT id, title, provider, model, status, parent_id, depth FROM tree ORDER BY path`, r.Host, roots, r.Live(), r.ID, r.FinishedAt)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentRow, error) {
		var a agentRow
		err := row.Scan(&a.ID, &a.Title, &a.Provider, &a.Model, &a.Status, &a.Parent, &a.Depth)
		return a, err
	})
}

func readHosts(ctx context.Context, p *pgxpool.Pool) ([]string, error) {
	rows, err := p.Query(ctx, "SELECT name FROM host_status ORDER BY online DESC, name")
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

// repoOption is a repository a workflow can always use: a main checkout some of the account's
// hosts reported (their workspaces' project root).
type repoOption struct {
	Directory string   `json:"directory"`
	Project   string   `json:"project"`
	Hosts     []string `json:"hosts"`
}

func readRepos(ctx context.Context, p *pgxpool.Pool) ([]repoOption, error) {
	rows, err := p.Query(ctx, `SELECT directory, max(project), array_agg(DISTINCT host ORDER BY host) FROM host_repositories
		GROUP BY directory ORDER BY lower(max(project)), directory`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repoOption, error) {
		var r repoOption
		err := row.Scan(&r.Directory, &r.Project, &r.Hosts)
		return r, err
	})
	if out == nil {
		out = []repoOption{}
	}
	return out, err
}

var workflowIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func renderWorkflows(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := workflowsTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// workflowsPage: GET /workflows: a new task, the account's workflows and their runs.
func workflowsPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		wfs, err := readWorkflows(ctx, v.pool, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows, err := v.pool.Query(ctx, runRowSelect+"ORDER BY r.created_at DESC LIMIT 100")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		runs, err := pgx.CollectRows(rows, scanRunRow)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hosts, err := readHosts(ctx, v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var tasks []workflowRow
		for _, wf := range wfs {
			if wf.Definition.Trigger == workflow.TriggerTask {
				tasks = append(tasks, wf)
			}
		}
		renderWorkflows(w, "list", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "Workflows": wfs, "Runs": runs,
			"Hosts": hosts, "Tasks": tasks, "Starting": workflow.Starting, "Dictation": dictationPath(cfg)})
	}
}

// workflowPage: GET /workflows/{id}: the editor.
func workflowPage(cfg Config, a *app, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !workflowIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		wfs, err := readWorkflows(r.Context(), v.pool, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(wfs) == 0 {
			http.NotFound(w, r)
			return
		}
		hosts, err := readHosts(r.Context(), v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		repos, err := readRepos(r.Context(), v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mb, err := readMailbox(r.Context(), v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		emails, err := readEmails(r.Context(), v.pool, "", 300)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		all, err := readWorkflows(r.Context(), v.pool, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		others := []workflowRow{} // the other workflows email starts: the preview says which email they would take
		for _, o := range all {
			if o.ID != id && o.Definition.Trigger == workflow.TriggerEmail && o.Definition.Email != nil {
				others = append(others, o)
			}
		}
		voiceStatus, err := readVoiceMemoStatus(r.Context(), cfg, v)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		renderWorkflows(w, "editor", map[string]any{"Version": cfg.Version, "Workflow": wfs[0], "Hosts": hosts, "Repos": repos,
			"VoiceStatus": voiceStatus, "EmailOn": a.mail != nil, "Mailbox": mb.Address, "Emails": previews(emails), "Others": others,
			"Dictation": dictationPath(cfg), "Providers": workflow.Providers()})
	}
}

// runPage: GET /workflow-runs/{id}: a run's steps, agents and what happened, and the note it waits on.
func runPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !workflowIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		ctx := r.Context()
		rows, err := v.pool.Query(ctx, runRowSelect+"WHERE r.id = $1", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		runs, err := pgx.CollectRows(rows, scanRunRow)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(runs) == 0 {
			http.NotFound(w, r)
			return
		}
		run := runs[0]
		// A model suggestion reads as its saved choice; its full text stays in SQL.
		rows, err = v.pool.Query(ctx, `SELECT e.kind,e.step,e.to_step,e.text,e.at,s.snapshot->'selected',s.token
            FROM workflow_run_events e LEFT JOIN workflow_model_suggestions s ON s.id=e.suggestion_id
            WHERE e.run_id=$1 ORDER BY e.id`, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runEvent, error) {
			var e runEvent
			var selected []byte
			var token *string
			err := row.Scan(&e.Kind, &e.Step, &e.ToStep, &e.Text, &e.At, &selected, &token)
			if err == nil && e.Kind == "model_suggestion" {
				e.Text = modelSuggestionLine(selected, token)
			}
			return e, err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows, err = v.pool.Query(ctx, "SELECT step, started_at, ended_at FROM workflow_run_step_visits WHERE run_id = $1 ORDER BY step, visit", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		visits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stepVisit, error) {
			var s stepVisit
			err := row.Scan(&s.Step, &s.Start, &s.End)
			return s, err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var archiveStatus, archiveError string
		err = v.pool.QueryRow(ctx, `SELECT COALESCE(c.status, ''), COALESCE(c.error, '')
            FROM workflow_runs r LEFT JOIN commands c ON c.id = r.archive_command WHERE r.id = $1`, id).Scan(&archiveStatus, &archiveError)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		agents, err := runAgents(ctx, v.pool, run)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if run.QueuedAtCreation {
			for i := range events {
				if events[i].Kind == "started" {
					events[i].Kind = "submitted"
				}
			}
		}
		var before []runEvent // what happened before the steps: the start and the project
		for _, e := range events {
			if e.Step == nil && e.Kind != "cancel" && e.Kind != "summarize" {
				before = append(before, e)
			}
		}
		emails, err := readEmails(ctx, v.pool, id, 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		voiceMemos, err := readVoiceMemos(ctx, v.pool, id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		slices.Reverse(emails) // oldest first, as a thread reads
		renderWorkflows(w, "run", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "Run": run, "Steps": stepViews(run, events, visits, cfg.Now()),
			"Agents": agents, "Before": before, "Events": events, "Emails": emails, "VoiceMemos": voiceMemos, "ArchiveStatus": archiveStatus, "ArchiveError": archiveError, "Dictation": dictationPath(cfg)})
	}
}

// modelSuggestionLine uses the saved choice, including labels that may have changed since.
func modelSuggestionLine(selected []byte, token *string) string {
	if token == nil {
		return "Model suggestion recorded"
	}
	var choice struct {
		Model      string `json:"model"`
		Label      string `json:"model_label"`
		Thinking   string `json:"thinking"`
		Login      string `json:"login"`
		LoginLabel string `json:"login_label"`
	}
	if json.Unmarshal(selected, &choice) != nil || choice.Model == "" {
		return "No eligible model for {{" + *token + "}}"
	}
	if choice.Label == "" {
		choice.Label = choice.Model
	}
	if choice.LoginLabel == "" {
		choice.LoginLabel = choice.Login
	}
	if choice.Thinking != "" {
		choice.Label += " (" + strings.ToUpper(choice.Thinking[:1]) + choice.Thinking[1:] + ")"
	}
	return fmt.Sprintf("Suggested %s on %s for {{%s}}", choice.Label, choice.LoginLabel, *token)
}

// Line is how one event reads on the run page.
func (e runEvent) Line() string {
	switch e.Kind {
	case "model_suggestion":
		return e.Text
	case "started":
		return "Started"
	case "summarize":
		return "Task summary: " + e.Text
	case "submitted": // the start of a run that was queued
		return "Submitted"
	case "queued":
		return "Queued: " + e.Text
	case "dequeued":
		return "Started: " + e.Text
	case "project":
		if e.Text == "none" {
			return "Project: none (the home directory)"
		}
		return "Project: " + e.Text
	case "workspace":
		return "The orchestrator started (" + e.Text + ")"
	case "next":
		return "Done: " + orDash(e.Text)
	case "done":
		return "Finished: " + orDash(e.Text)
	case "skip":
		return "Skipped: " + orDash(e.Text)
	case "back":
		return "Sent back: " + orDash(e.Text)
	case "ask":
		return "Asked Zach"
	case "reply":
		return "Zach replied: " + e.Text
	case "message":
		return e.Text
	case "fail":
		return "Failed: " + e.Text
	case "cancel":
		return "Canceled"
	case "error", "stalled", "continuation", "resume":
		return e.Text
	case "start_retry":
		return "Start failed; retry queued: " + e.Text
	case "start_retry_sent":
		return "Retry sent: " + e.Text
	}
	return e.Kind + " " + e.Text
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func (a *app) resumeRun(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := a.wf.Resume(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
