package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// Workflows. Writes go through the JSON API below; pages read what they show through the
// account's own role, like everything else. A host's agents move runs with the host's key
// (`intelligence workflow`, POST /api/workflow-runs/{id}/moves).

//go:embed workflows.html
var workflowsHTML string

var workflowsTmpl = template.Must(template.New("workflows").Funcs(template.FuncMap{
	"short": short, "ago": ago, "markdown": markdown, "plural": plural, "emailSummary": emailSummary,
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		return template.JS(b), err //nolint:gosec // JSON from the server's own values, in a script
	},
	"add": func(a, b int) int { return a + b },
	"kind": func(k string) string {
		return map[string]string{"step": "Step", "gate": "Gate", "signoff": "Sign-off"}[k]
	},
}).Parse(workflowsHTML + layoutHTML))

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
	switch {
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
		w.WriteHeader(http.StatusNoContent)
	}
}

// moveRun: POST /api/workflow-runs/{id}/moves, from a host's agent with the host's key
// (`intelligence workflow`). It answers {"message"}: what the agent should read next.
func (a *app) moveRun(w http.ResponseWriter, r *http.Request) {
	key, _ := bearerOf(r)
	host, err := a.store.HostOfKey(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if host == "" {
		http.Error(w, "bad or missing host key", http.StatusUnauthorized)
		return
	}
	var m workflow.Move
	if !decodeJSON(w, r, &m) {
		return
	}
	msg, err := a.wf.MoveRun(r.Context(), host, r.PathValue("id"), m)
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
			msg = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// --- reads, through the account's role ---

type workflowRow struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Version     int                 `json:"version"`
	IsDefault   bool                `json:"isDefault"`
	Definition  workflow.Definition `json:"definition"`
}

func readWorkflows(ctx context.Context, p *pgxpool.Pool, id string) ([]workflowRow, error) {
	rows, err := p.Query(ctx, `SELECT w.id::text, w.name, w.description, w.version, w.is_default, v.definition FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE $1 = '' OR w.id::text = $1 ORDER BY w.is_default DESC, w.created_at, w.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (workflowRow, error) {
		var w workflowRow
		var def []byte
		err := row.Scan(&w.ID, &w.Name, &w.Description, &w.Version, &w.IsDefault, &def)
		if err == nil {
			err = json.Unmarshal(def, &w.Definition)
		}
		return w, err
	})
}

// runRow is a run as the pages show it.
type runRow struct {
	ID, Title, Input, Status, Host, Repo, Branch, Workflow, WorkflowID string
	WorkspaceID, Orchestrator, ProjectAgent, Note, Error               string
	Step, Version                                                      int
	Steps                                                              []workflow.Step
	CreatedAt, UpdatedAt                                               time.Time
	FinishedAt                                                         *time.Time
}

const runRowSelect = `SELECT r.id::text, r.title, r.input, r.status, r.host, r.repo, r.branch, w.name, r.workflow_id::text, r.workspace_id,
	r.orchestrator_id, r.project_agent_id, r.note, r.error, r.step, r.version, v.definition -> 'steps', r.created_at, r.updated_at, r.finished_at
	FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version `

func scanRunRow(row pgx.CollectableRow) (runRow, error) {
	var r runRow
	var steps []byte
	err := row.Scan(&r.ID, &r.Title, &r.Input, &r.Status, &r.Host, &r.Repo, &r.Branch, &r.Workflow, &r.WorkflowID, &r.WorkspaceID,
		&r.Orchestrator, &r.ProjectAgent, &r.Note, &r.Error, &r.Step, &r.Version, &steps, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	if err == nil {
		err = json.Unmarshal(steps, &r.Steps)
	}
	return r, err
}

// Where reads as: what a run is doing now.
func (r runRow) Where() string {
	switch r.Status {
	case workflow.FindingProject:
		return "finding the project"
	case workflow.StartingOrchestrator:
		return "starting the orchestrator"
	case workflow.Running, workflow.Waiting:
		if r.Step < len(r.Steps) {
			return r.Steps[r.Step].Title
		}
	}
	return ""
}

// StatusLabel and StatusClass show a run's status as the pages' badges do.
func (r runRow) StatusLabel() string {
	switch r.Status {
	case workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running:
		return "running"
	case workflow.Waiting:
		return "needs you"
	}
	return r.Status
}

func (r runRow) StatusClass() string {
	switch r.Status {
	case workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running:
		return "st-running"
	case workflow.Waiting:
		return "st-needs"
	case workflow.Succeeded:
		return "st-done"
	case workflow.Failed:
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
	Times  int // how often the orchestrator was handed it
	Events []runEvent
}

func stepViews(r runRow, events []runEvent) []stepView {
	out := make([]stepView, len(r.Steps))
	for i, s := range r.Steps {
		out[i] = stepView{Step: s, N: i + 1, State: "pending", Label: "waiting"}
	}
	for _, e := range events {
		if e.Kind == "workspace" && len(out) > 0 {
			out[0].Times++
		}
		if e.ToStep != nil && *e.ToStep < len(out) {
			out[*e.ToStep].Times++
		}
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
	if (r.Status == workflow.Running || r.Status == workflow.Waiting) && r.Step < len(out) {
		v := &out[r.Step]
		v.State, v.Label = "running", "running"
		if r.Status == workflow.Waiting {
			v.State, v.Label = "asking", "asking you"
		}
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
	rows, err := p.Query(ctx, `WITH RECURSIVE tree AS (
		SELECT a.id, a.title, a.provider, a.model, a.status, a.parent_id, 0 AS depth,
			ARRAY[lpad(array_position($2::text[], a.id)::text, 3, '0')] AS path
		FROM agents a WHERE a.host = $1 AND a.id = ANY($2)
		UNION ALL
		SELECT a.id, a.title, a.provider, a.model, a.status, a.parent_id, t.depth + 1,
			t.path || (coalesce(to_char(a.created_at AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS'), '') || a.id)
		FROM agents a JOIN tree t ON a.parent_id = t.id WHERE a.host = $1 AND t.depth < 6)
		SELECT id, title, provider, model, status, parent_id, depth FROM tree ORDER BY path`, r.Host, roots)
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
	rows, err := p.Query(ctx, `SELECT project_root, max(project), array_agg(DISTINCT host ORDER BY host) FROM workspaces
		WHERE project_root <> '' GROUP BY project_root ORDER BY lower(max(project)), project_root`)
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
			"Hosts": hosts, "Tasks": tasks, "Starting": workflow.Starting})
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
		renderWorkflows(w, "editor", map[string]any{"Version": cfg.Version, "Workflow": wfs[0], "Hosts": hosts, "Repos": repos,
			"EmailOn": a.mail != nil, "Mailbox": mb.Address, "Emails": previews(emails), "Others": others,
			"Dictation": dictationPath(cfg)})
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
		rows, err = v.pool.Query(ctx, "SELECT kind, step, to_step, text, at FROM workflow_run_events WHERE run_id = $1 ORDER BY id", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runEvent, error) {
			var e runEvent
			err := row.Scan(&e.Kind, &e.Step, &e.ToStep, &e.Text, &e.At)
			return e, err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		agents, err := runAgents(ctx, v.pool, run)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var before []runEvent // what happened before the steps: the start and the project
		for _, e := range events {
			if e.Step == nil && e.Kind != "cancel" {
				before = append(before, e)
			}
		}
		emails, err := readEmails(ctx, v.pool, id, 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slices.Reverse(emails) // oldest first, as a thread reads
		renderWorkflows(w, "run", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "Run": run, "Steps": stepViews(run, events),
			"Agents": agents, "Before": before, "Events": events, "Emails": emails})
	}
}

// eventLine is how one event reads on the run page.
func (e runEvent) Line() string {
	switch e.Kind {
	case "started":
		return "Started"
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
	case "error":
		return e.Text
	}
	return e.Kind + " " + e.Text
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
