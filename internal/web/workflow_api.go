package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/workflow"
)

const (
	maxWorkflowAPIBody      = 16 << 20 // allows inline attachments (up to 4 MiB each, base64); text fields capped at 1 MiB
	defaultSSEInterval      = 15 * time.Second
	defaultSSETerminalGrace = 3 * time.Second
)

// --- Browser-session key management (never reachable with an API key) ---

// createWorkflowAPIKey: POST /api/workflows/{id}/api-keys {"name": "..."}.
func (a *app) createWorkflowAPIKey(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &req) {
			return
		}
		key, err := a.store.CreateWorkflowAPIKey(r.Context(), v.acct, r.PathValue("id"), req.Name)
		if err != nil {
			var bad *accounts.BadInputError
			switch {
			case errors.As(err, &bad):
				apiError(w, http.StatusBadRequest, err.Error())
			case errors.Is(err, accounts.ErrNoSuchWorkflow):
				apiError(w, http.StatusNotFound, err.Error())
			default:
				log.Printf("create workflow api key: %v", err)
				apiError(w, http.StatusInternalServerError, "could not create API key")
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(key)
	}
}

// revokeWorkflowAPIKey: DELETE /api/workflows/{id}/api-keys/{keyId}.
func (a *app) revokeWorkflowAPIKey(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := a.store.RevokeWorkflowAPIKey(r.Context(), v.acct, r.PathValue("id"), r.PathValue("keyId"))
		if err != nil {
			log.Printf("revoke workflow api key: %v", err)
			apiError(w, http.StatusInternalServerError, "could not revoke API key")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such API key for this workflow")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// workflowAPIKeyRow is one row of workflow_api_keys as shown on the workflow editor page.
type workflowAPIKeyRow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func readWorkflowAPIKeys(ctx context.Context, p *pgxpool.Pool, workflowID string) ([]workflowAPIKeyRow, error) {
	rows, err := p.Query(ctx, `SELECT id::text, name, key_prefix, created_at, last_used_at, revoked_at
		FROM workflow_api_keys WHERE workflow_id = $1 ORDER BY created_at DESC, id DESC`, workflowID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (workflowAPIKeyRow, error) {
		var k workflowAPIKeyRow
		err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
		return k, err
	})
	if out == nil {
		out = []workflowAPIKeyRow{}
	}
	return out, err
}

// --- Scoped workflow API-key middleware and handlers ---

type apiCaller struct {
	auth accounts.WorkflowAPIKeyAuth
	pool *pgxpool.Pool // account's read-only role (RLS enforced)
	reg  *fleet.Registry
}

func (c apiCaller) actorCtx(ctx context.Context) context.Context {
	return workflow.WithAPIActor(ctx, workflow.APIActor{KeyID: c.auth.KeyID, KeyName: c.auth.KeyName})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func apiWorkflowError(w http.ResponseWriter, err error) {
	var ref *workflow.Refusal
	var badCommand *fleet.BadCommandError
	switch {
	case errors.As(err, &badCommand):
		apiError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workflow.ErrNotFound):
		apiError(w, http.StatusNotFound, "no such run in this workflow")
	case errors.As(err, &ref) && ref.Forbidden:
		apiError(w, http.StatusForbidden, ref.Msg)
	case errors.As(err, &ref) && ref.Conflict:
		apiError(w, http.StatusConflict, ref.Msg)
	case errors.As(err, &ref):
		apiError(w, http.StatusBadRequest, ref.Msg)
	default:
		log.Printf("workflow api: %v", err)
		apiError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (a *app) workflowAPI(h func(apiCaller) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !mcpOrigin(a.cfg, r) {
			apiError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(key) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="intelligence-workflow-api"`)
			apiError(w, http.StatusUnauthorized, "API key required")
			return
		}
		auth, err := a.store.AuthenticateWorkflowAPIKey(r.Context(), strings.TrimSpace(key))
		if err != nil {
			switch {
			case errors.Is(err, accounts.ErrNoSession):
				w.Header().Set("WWW-Authenticate", `Bearer realm="intelligence-workflow-api", error="invalid_token"`)
				apiError(w, http.StatusUnauthorized, "invalid API key")
			case errors.Is(err, accounts.ErrKeyRevoked):
				w.Header().Set("WWW-Authenticate", `Bearer realm="intelligence-workflow-api", error="invalid_token"`)
				apiError(w, http.StatusUnauthorized, "API key has been revoked")
			case errors.Is(err, accounts.ErrWorkflowDeleted):
				apiError(w, http.StatusForbidden, "workflow has been deleted")
			default:
				log.Printf("workflow api auth: %v", err)
				apiError(w, http.StatusInternalServerError, "authentication unavailable")
			}
			return
		}
		pool, err := a.store.Reader(r.Context(), auth.Account)
		if err != nil {
			log.Printf("workflow api reader: %v", err)
			apiError(w, http.StatusInternalServerError, "reader unavailable")
			return
		}
		h(apiCaller{auth: auth, pool: pool, reg: fleet.New(pool)})(w, r)
	}
}

func (a *app) workflowAPIWrite(h func(apiCaller) http.HandlerFunc) http.HandlerFunc {
	return a.workflowAPI(func(c apiCaller) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !requireJSON(w, r) {
				return
			}
			h(c)(w, r)
		}
	})
}

func decodeAPIBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apiError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		apiError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apiError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		apiError(w, http.StatusBadRequest, "request body must contain a single JSON object")
		return false
	}
	return true
}

// --- JSON response shapes ---

type apiRunSummary struct {
	ID                string     `json:"id"`
	WorkflowID        string     `json:"workflowId"`
	WorkflowName      string     `json:"workflowName"`
	Version           int        `json:"version"`
	Status            string     `json:"status"`
	Title             string     `json:"title"`
	TaskSummary       string     `json:"taskSummary,omitempty"`
	Host              string     `json:"host"`
	Repo              string     `json:"repo,omitempty"`
	Branch            string     `json:"branch,omitempty"`
	WorkspaceID       string     `json:"workspaceId,omitempty"`
	OrchestratorID    string     `json:"orchestratorId,omitempty"`
	ProjectAgentID    string     `json:"projectAgentId,omitempty"`
	Step              int        `json:"step"`
	CurrentStepID     string     `json:"currentStepId,omitempty"`
	CurrentStepTitle  string     `json:"currentStepTitle,omitempty"`
	Note              string     `json:"note,omitempty"`
	Error             string     `json:"error,omitempty"`
	QueueReason       string     `json:"queueReason,omitempty"`
	QueuePosition     int        `json:"queuePosition,omitempty"`
	CurrentActivity   string     `json:"currentActivity,omitempty"`
	ActivityUpdatedAt *time.Time `json:"activityUpdatedAt,omitempty"`
	ActivityETA       *time.Time `json:"activityEta,omitempty"`
	ActivityWaitingOn string     `json:"activityWaitingOn,omitempty"`
	TriggerSource     string     `json:"triggerSource"`
	APIKeyID          string     `json:"apiKeyId,omitempty"`
	APIKeyName        string     `json:"apiKeyName,omitempty"`
	RetriedFromRunID  string     `json:"retriedFromRunId,omitempty"`
	RetriedToRunID    string     `json:"retriedToRunId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
}

type apiStepState struct {
	Index               int     `json:"index"`
	ID                  string  `json:"id"`
	Kind                string  `json:"kind"`
	Title               string  `json:"title"`
	RequireHumanSignoff bool    `json:"requireHumanSignoff,omitempty"`
	State               string  `json:"state"`
	Label               string  `json:"label"`
	SpentSeconds        float64 `json:"spentSeconds"`
	Visits              int     `json:"visits"`
}

type apiRunEvent struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Step       *int      `json:"step,omitempty"`
	ToStep     *int      `json:"toStep,omitempty"`
	Text       string    `json:"text"`
	At         time.Time `json:"at"`
	APIKeyID   string    `json:"apiKeyId,omitempty"`
	APIKeyName string    `json:"apiKeyName,omitempty"`
}

type apiRunAgent struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	ParentID string `json:"parentId,omitempty"`
	Depth    int    `json:"depth"`
}

type apiRunDetail struct {
	apiRunSummary
	Input  string         `json:"input"`
	Steps  []apiStepState `json:"steps"`
	Events []apiRunEvent  `json:"events"`
	Agents []apiRunAgent  `json:"agents"`
}

type apiTranscriptSummary struct {
	apiRunAgent
	Epoch      string     `json:"epoch,omitempty"`
	SyncedAt   *time.Time `json:"syncedAt,omitempty"`
	EntryCount int        `json:"entryCount"`
}

type apiTranscriptEntry struct {
	SeqStart int             `json:"seqStart"`
	SeqEnd   int             `json:"seqEnd"`
	TurnID   string          `json:"turnId,omitempty"`
	Provider string          `json:"provider,omitempty"`
	At       *time.Time      `json:"at,omitempty"`
	Kind     string          `json:"kind"`
	Tool     string          `json:"tool,omitempty"`
	Status   string          `json:"status,omitempty"`
	Text     string          `json:"text,omitempty"`
	Item     json.RawMessage `json:"item"`
}

// apiEntry is a synced transcript entry as the workflow API shows it.
func apiEntry(e fleet.Entry) apiTranscriptEntry {
	out := apiTranscriptEntry{SeqStart: e.SeqStart, SeqEnd: e.SeqEnd, TurnID: e.TurnID, Provider: e.Provider,
		Kind: e.Kind, Tool: e.Tool, Status: e.Status, Text: e.Text, Item: e.Item}
	if !e.At.IsZero() {
		at := e.At.UTC()
		out.At = &at
	}
	return out
}

func runSummaryFromRow(r runRow) apiRunSummary {
	triggerSource := "workflow"
	if r.APIKeyID != "" {
		triggerSource = "api"
	}
	s := apiRunSummary{
		ID:                r.ID,
		WorkflowID:        r.WorkflowID,
		WorkflowName:      r.Workflow,
		Version:           r.Version,
		Status:            r.Status,
		Title:             r.Title,
		TaskSummary:       r.TaskSummary,
		Host:              r.Host,
		Repo:              r.Repo,
		Branch:            r.Branch,
		WorkspaceID:       r.WorkspaceID,
		OrchestratorID:    r.Orchestrator,
		ProjectAgentID:    r.ProjectAgent,
		Step:              r.Step,
		Note:              r.Note,
		Error:             r.Error,
		QueueReason:       r.QueueReason,
		QueuePosition:     r.Position,
		CurrentActivity:   r.CurrentActivity,
		ActivityUpdatedAt: r.ActivityUpdatedAt,
		ActivityETA:       r.ActivityETA,
		ActivityWaitingOn: r.ActivityWaitingOn,
		TriggerSource:     triggerSource,
		APIKeyID:          r.APIKeyID,
		APIKeyName:        r.APIKeyName,
		RetriedFromRunID:  r.RetriedFrom,
		RetriedToRunID:    r.RetriedTo,
		CreatedAt:         r.CreatedAt.UTC(),
		UpdatedAt:         r.UpdatedAt.UTC(),
		FinishedAt:        r.FinishedAt,
	}
	if r.Step >= 0 && r.Step < len(r.Steps) && (r.Status == workflow.Running || r.Status == workflow.Waiting || r.Status == workflow.Stalled) {
		s.CurrentStepID = r.Steps[r.Step].ID
		s.CurrentStepTitle = r.Steps[r.Step].Title
	}
	return s
}

// readScopedRunRow is run runID if it belongs to workflowID (found=false otherwise).
func readScopedRunRow(ctx context.Context, p *pgxpool.Pool, workflowID, runID string) (runRow, bool, error) {
	if !workflowIDRe.MatchString(runID) {
		return runRow{}, false, nil
	}
	rows, err := p.Query(ctx, runRowSelect+"WHERE r.id = $1 AND r.workflow_id = $2", runID, workflowID)
	if err != nil {
		return runRow{}, false, err
	}
	list, err := pgx.CollectRows(rows, scanRunRow)
	if err != nil || len(list) == 0 {
		return runRow{}, false, err
	}
	return list[0], true, nil
}

func verifyScopedRun(ctx context.Context, p *pgxpool.Pool, workflowID, runID string) (bool, error) {
	if !workflowIDRe.MatchString(runID) {
		return false, nil
	}
	var exists bool
	err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_runs WHERE id = $1 AND workflow_id = $2)`, runID, workflowID).Scan(&exists)
	return exists, err
}

func readRunEventsWithID(ctx context.Context, p *pgxpool.Pool, runID string, afterID int64) ([]apiRunEvent, []runEvent, error) {
	rows, err := p.Query(ctx, `SELECT e.id, e.kind, e.step, e.to_step, e.text, e.at, s.snapshot->'selected', s.token,
		coalesce(e.api_key_id::text, ''), coalesce(ek.name, '')
		FROM workflow_run_events e
		LEFT JOIN workflow_model_suggestions s ON s.id = e.suggestion_id
		LEFT JOIN workflow_api_keys ek ON ek.id = e.api_key_id
		WHERE e.run_id = $1 AND e.id > $2 ORDER BY e.id`, runID, afterID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	apiEvents := []apiRunEvent{}
	viewEvents := []runEvent{}
	for rows.Next() {
		var ev apiRunEvent
		var selected []byte
		var token *string
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.Step, &ev.ToStep, &ev.Text, &ev.At, &selected, &token, &ev.APIKeyID, &ev.APIKeyName); err != nil {
			return nil, nil, err
		}
		if ev.Kind == "model_suggestion" {
			ev.Text = modelSuggestionLine(selected, token)
		}
		ev.At = ev.At.UTC()
		apiEvents = append(apiEvents, ev)
		viewEvents = append(viewEvents, runEvent{Kind: ev.Kind, Step: ev.Step, ToStep: ev.ToStep, Text: ev.Text, At: ev.At, APIKeyID: ev.APIKeyID, APIKeyName: ev.APIKeyName})
	}
	return apiEvents, viewEvents, rows.Err()
}

// --- Route handlers ---

// apiGetWorkflow: GET /api/workflow-api/workflow.
func (a *app) apiGetWorkflow(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wfs, err := readWorkflows(r.Context(), c.pool, c.auth.WorkflowID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read workflow")
			return
		}
		if len(wfs) == 0 {
			apiError(w, http.StatusNotFound, "workflow not found")
			return
		}
		wf := wfs[0]
		type stepInfo struct {
			ID                  string `json:"id"`
			Kind                string `json:"kind"`
			Title               string `json:"title"`
			RequireHumanSignoff bool   `json:"requireHumanSignoff,omitempty"`
		}
		steps := make([]stepInfo, len(wf.Definition.Steps))
		for i, s := range wf.Definition.Steps {
			steps[i] = stepInfo{ID: s.ID, Kind: s.Kind, Title: s.Title, RequireHumanSignoff: s.RequireHumanSignoff}
		}
		writeJSON(w, map[string]any{
			"id":          wf.ID,
			"name":        wf.Name,
			"description": wf.Description,
			"version":     wf.Version,
			"host":        wf.Definition.Host,
			"trigger":     wf.Definition.Trigger,
			"apiTrigger":  wf.Definition.APITrigger,
			"priority":    wf.Definition.EffectivePriority(),
			"steps":       steps,
		})
	}
}

// apiTriggerRun: POST /api/workflow-api/runs.
func (a *app) apiTriggerRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.auth.APITriggerEnabled {
			apiError(w, http.StatusForbidden, "API trigger is disabled for this workflow")
			return
		}
		var req workflow.APIRunRequest
		if !decodeAPIBody(w, r, maxWorkflowAPIBody, &req) {
			return
		}
		if len(req.Input) > workflow.MaxAPITextBytes {
			apiError(w, http.StatusRequestEntityTooLarge, "input exceeds 1 MiB limit")
			return
		}
		if hdr := strings.TrimSpace(r.Header.Get("Idempotency-Key")); hdr != "" {
			req.IdempotencyKey = hdr
		}
		res, err := a.wf.StartAPIRun(c.actorCtx(r.Context()), c.auth.Account.ID, c.auth.WorkflowID, c.auth.KeyID, req)
		if err != nil {
			apiWorkflowError(w, err)
			return
		}
		row, found, err := readScopedRunRow(r.Context(), c.pool, c.auth.WorkflowID, res.RunID)
		if err != nil || !found {
			apiError(w, http.StatusInternalServerError, "run created but could not be read")
			return
		}
		w.Header().Set("Location", "/api/workflow-api/runs/"+res.RunID)
		if res.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, runSummaryFromRow(row))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(runSummaryFromRow(row))
	}
}

func parseAPIStatusFilter(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return nil, nil
	}
	valid := map[string][]string{
		workflow.Queued:               {workflow.Queued},
		workflow.FindingProject:       {workflow.FindingProject},
		workflow.StartingOrchestrator: {workflow.StartingOrchestrator},
		workflow.Running:              {workflow.Running},
		workflow.Waiting:              {workflow.Waiting},
		workflow.Stalled:              {workflow.Stalled},
		workflow.Succeeded:            {workflow.Succeeded},
		workflow.Failed:               {workflow.Failed},
		workflow.Canceled:             {workflow.Canceled},
		"active":                      {workflow.Queued, workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running, workflow.Waiting, workflow.Stalled},
		"completed":                   {workflow.Succeeded},
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		mapped, ok := valid[p]
		if !ok {
			return nil, fmt.Errorf("unknown status %q", p)
		}
		for _, s := range mapped {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func encodeRunCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeRunCursor(raw string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", errors.New("invalid cursor")
	}
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok || !workflowIDRe.MatchString(id) {
		return time.Time{}, "", errors.New("invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", errors.New("invalid cursor")
	}
	return t, id, nil
}

// apiListRuns: GET /api/workflow-api/runs?status=&limit=&cursor=.
func (a *app) apiListRuns(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		statuses, err := parseAPIStatusFilter(r.URL.Query().Get("status"))
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		limit := 25
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n < 1 || n > 100 {
				apiError(w, http.StatusBadRequest, "limit must be between 1 and 100")
				return
			}
			limit = int(n)
		}
		var cursorAt *time.Time
		var cursorID string
		if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
			t, id, err := decodeRunCursor(raw)
			if err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
			cursorAt, cursorID = &t, id
		}
		if statuses == nil {
			statuses = []string{}
		}
		var total int
		if err := c.pool.QueryRow(r.Context(), `SELECT count(*) FROM workflow_runs r
			WHERE r.workflow_id = $1 AND (cardinality($2::text[]) = 0 OR r.status = ANY($2))`,
			c.auth.WorkflowID, statuses).Scan(&total); err != nil {
			apiError(w, http.StatusInternalServerError, "could not count runs")
			return
		}
		where := `WHERE r.workflow_id = $1 AND (cardinality($2::text[]) = 0 OR r.status = ANY($2)) `
		args := []any{c.auth.WorkflowID, statuses, limit + 1}
		if cursorAt != nil {
			where += `AND (r.created_at, r.id) < ($4, $5::uuid) `
			args = append(args, *cursorAt, cursorID)
		}
		rows, err := c.pool.Query(r.Context(), runRowSelect+where+`ORDER BY r.created_at DESC, r.id DESC LIMIT $3`, args...)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not list runs")
			return
		}
		list, err := pgx.CollectRows(rows, scanRunRow)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read runs")
			return
		}
		nextCursor := ""
		if len(list) > limit {
			list = list[:limit]
			last := list[len(list)-1]
			nextCursor = encodeRunCursor(last.CreatedAt, last.ID)
		}
		out := make([]apiRunSummary, len(list))
		for i, rr := range list {
			out[i] = runSummaryFromRow(rr)
		}
		writeJSON(w, map[string]any{
			"runs":       out,
			"total":      total,
			"nextCursor": nextCursor,
		})
	}
}

// apiGetRun: GET /api/workflow-api/runs/{id}.
func (a *app) apiGetRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		row, found, err := readScopedRunRow(ctx, c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run")
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		apiEvents, viewEvents, err := readRunEventsWithID(ctx, c.pool, id, 0)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run events")
			return
		}
		vrows, err := c.pool.Query(ctx, "SELECT step, started_at, ended_at FROM workflow_run_step_visits WHERE run_id = $1 ORDER BY step, visit", id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read step visits")
			return
		}
		visits, err := pgx.CollectRows(vrows, func(row pgx.CollectableRow) (stepVisit, error) {
			var s stepVisit
			err := row.Scan(&s.Step, &s.Start, &s.End)
			return s, err
		})
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read step visits")
			return
		}
		svs := stepViews(row, viewEvents, visits, a.cfg.Now())
		steps := make([]apiStepState, len(svs))
		for i, sv := range svs {
			reqHuman := i < len(row.Steps) && row.Steps[i].RequireHumanSignoff
			steps[i] = apiStepState{
				Index:               i,
				ID:                  sv.ID,
				Kind:                sv.Kind,
				Title:               sv.Title,
				RequireHumanSignoff: reqHuman,
				State:               sv.State,
				Label:               sv.Label,
				SpentSeconds:        sv.Spent.Seconds(),
				Visits:              len(sv.Visits),
			}
		}
		agentRows, err := runAgents(ctx, c.pool, row)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run agents")
			return
		}
		agents := make([]apiRunAgent, len(agentRows))
		for i, ag := range agentRows {
			agents[i] = apiRunAgent{
				ID: ag.ID, Title: ag.Title, Provider: ag.Provider, Model: ag.Model,
				Status: ag.Status, ParentID: ag.Parent, Depth: ag.Depth,
			}
		}
		writeJSON(w, apiRunDetail{
			apiRunSummary: runSummaryFromRow(row),
			Input:         row.Input,
			Steps:         steps,
			Events:        apiEvents,
			Agents:        agents,
		})
	}
}

// apiListRunTranscripts: GET /api/workflow-api/runs/{id}/transcripts.
func (a *app) apiListRunTranscripts(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		row, found, err := readScopedRunRow(ctx, c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run")
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		agentRows, err := runAgents(ctx, c.pool, row)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run agents")
			return
		}
		out := make([]apiTranscriptSummary, len(agentRows))
		for i, ag := range agentRows {
			ts := apiTranscriptSummary{
				apiRunAgent: apiRunAgent{
					ID: ag.ID, Title: ag.Title, Provider: ag.Provider, Model: ag.Model,
					Status: ag.Status, ParentID: ag.Parent, Depth: ag.Depth,
				},
			}
			var epoch string
			var synced time.Time
			var count int
			err := c.pool.QueryRow(ctx, `SELECT t.epoch, t.synced_at,
				(SELECT count(*) FROM transcript_entries e WHERE e.host = t.host AND e.agent_id = t.agent_id)::int
				FROM transcripts t WHERE t.host = $1 AND t.agent_id = $2`, row.Host, ag.ID).Scan(&epoch, &synced, &count)
			if err == nil {
				s := synced.UTC()
				ts.Epoch = epoch
				ts.SyncedAt = &s
				ts.EntryCount = count
			} else if !errors.Is(err, pgx.ErrNoRows) {
				apiError(w, http.StatusInternalServerError, "could not read transcript metadata")
				return
			}
			out[i] = ts
		}
		writeJSON(w, map[string]any{"runId": id, "host": row.Host, "transcripts": out})
	}
}

// apiGetRunTranscript: GET /api/workflow-api/runs/{id}/transcripts/{agentId}?from_seq=&after_seq=&limit=.
func (a *app) apiGetRunTranscript(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		agentID := r.PathValue("agentId")
		row, found, err := readScopedRunRow(ctx, c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run")
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		agentRows, err := runAgents(ctx, c.pool, row)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run agents")
			return
		}
		belongs := false
		for _, ag := range agentRows {
			if ag.ID == agentID {
				belongs = true
				break
			}
		}
		if !belongs {
			apiError(w, http.StatusNotFound, "no such agent in this run")
			return
		}
		fromSeq := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("after_seq")); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "after_seq must be a non-negative 32-bit integer")
				return
			}
			fromSeq = int(n) + 1
		} else if raw := strings.TrimSpace(r.URL.Query().Get("from_seq")); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "from_seq must be a non-negative 32-bit integer")
				return
			}
			fromSeq = int(n)
		}
		limit := 500
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n < 1 || n > 2000 {
				apiError(w, http.StatusBadRequest, "limit must be between 1 and 2000")
				return
			}
			limit = int(n)
		}
		tr, err := c.reg.TranscriptPage(ctx, row.Host, agentID, fromSeq, limit+1)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, map[string]any{
				"runId": id, "agentId": agentID, "host": row.Host,
				"epoch": "", "entries": []apiTranscriptEntry{}, "hasMore": false,
			})
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read transcript")
			return
		}
		epoch, syncedAt := tr.Epoch, tr.SyncedAt
		entries := make([]apiTranscriptEntry, 0, len(tr.Entries))
		for _, e := range tr.Entries {
			entries = append(entries, apiEntry(e))
		}
		hasMore := false
		nextSeq := 0
		if len(entries) > limit {
			hasMore = true
			entries = entries[:limit]
		}
		if len(entries) > 0 {
			nextSeq = entries[len(entries)-1].SeqStart
		}
		writeJSON(w, map[string]any{
			"runId":    id,
			"agentId":  agentID,
			"host":     row.Host,
			"epoch":    epoch,
			"syncedAt": syncedAt.UTC(),
			"entries":  entries,
			"hasMore":  hasMore,
			"nextSeq":  nextSeq,
		})
	}
}

// --- Real-time SSE streaming with composite resumable cursor ---

// streamCursor tracks the last delivered run event ID and per-agent transcript
// position so SSE reconnects (Last-Event-ID) skip already-delivered events and
// transcript entries without rereading from sequence 0.
//
// Wire format:
//
//	v1:<eventID>
//	v1:<eventID>;<agentID>=<epoch>:<seqStart>:<seqEnd>:<textLen>,...
type agentStreamPos struct {
	epoch    string
	seqStart int
	seqEnd   int
	textLen  int
}

type streamCursor struct {
	eventID int64
	agents  map[string]*agentStreamPos
}

func (sc *streamCursor) encode() string {
	var b strings.Builder
	fmt.Fprintf(&b, "v1:%d", sc.eventID)
	if len(sc.agents) == 0 {
		return b.String()
	}
	ids := make([]string, 0, len(sc.agents))
	for id := range sc.agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	b.WriteByte(';')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		p := sc.agents[id]
		fmt.Fprintf(&b, "%s=%s:%d:%d:%d", id, p.epoch, p.seqStart, p.seqEnd, p.textLen)
	}
	return b.String()
}

func parseStreamCursor(r *http.Request) (*streamCursor, error) {
	sc := &streamCursor{agents: map[string]*agentStreamPos{}}
	if raw := strings.TrimSpace(r.URL.Query().Get("after_event_id")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, errors.New("after_event_id must be a non-negative integer")
		}
		sc.eventID = n
		return sc, nil
	}
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return sc, nil
	}
	if rest, ok := strings.CutPrefix(raw, "v1:"); ok {
		evPart, agPart, hasAgents := strings.Cut(rest, ";")
		n, err := strconv.ParseInt(evPart, 10, 64)
		if err != nil || n < 0 {
			return nil, errors.New("invalid Last-Event-ID")
		}
		sc.eventID = n
		if hasAgents && agPart != "" {
			for _, item := range strings.Split(agPart, ",") {
				agID, spec, ok := strings.Cut(item, "=")
				if !ok || agID == "" {
					return nil, errors.New("invalid Last-Event-ID")
				}
				parts := strings.Split(spec, ":")
				if len(parts) != 4 {
					return nil, errors.New("invalid Last-Event-ID")
				}
				seqStart, err1 := strconv.ParseInt(parts[1], 10, 32)
				seqEnd, err2 := strconv.ParseInt(parts[2], 10, 32)
				textLen, err3 := strconv.ParseInt(parts[3], 10, 32)
				if err1 != nil || err2 != nil || err3 != nil || seqStart < 0 || seqEnd < 0 || textLen < 0 {
					return nil, errors.New("invalid Last-Event-ID")
				}
				sc.agents[agID] = &agentStreamPos{
					epoch:    parts[0],
					seqStart: int(seqStart),
					seqEnd:   int(seqEnd),
					textLen:  int(textLen),
				}
			}
		}
		return sc, nil
	}
	// Legacy event:<id> or plain numeric ID.
	raw = strings.TrimPrefix(raw, "event:")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, errors.New("invalid Last-Event-ID")
	}
	sc.eventID = n
	return sc, nil
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func runTerminal(status string) bool {
	return status == workflow.Succeeded || status == workflow.Failed || status == workflow.Canceled
}

func runSnapshotChanged(a, b apiRunSummary) bool {
	if a.Status != b.Status || a.Step != b.Step || a.Note != b.Note || a.Error != b.Error ||
		a.TaskSummary != b.TaskSummary || a.CurrentActivity != b.CurrentActivity ||
		a.OrchestratorID != b.OrchestratorID || a.ProjectAgentID != b.ProjectAgentID ||
		a.QueueReason != b.QueueReason || a.QueuePosition != b.QueuePosition ||
		a.ActivityWaitingOn != b.ActivityWaitingOn || a.Title != b.Title ||
		a.Branch != b.Branch || a.WorkspaceID != b.WorkspaceID {
		return true
	}
	if (a.ActivityETA == nil) != (b.ActivityETA == nil) {
		return true
	}
	if a.ActivityETA != nil && !a.ActivityETA.Equal(*b.ActivityETA) {
		return true
	}
	return false
}

// apiStreamRun: GET /api/workflow-api/runs/{id}/stream (SSE, resumable).
func (a *app) apiStreamRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		cursor, err := parseStreamCursor(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		includeTranscripts := r.URL.Query().Get("include_transcripts") != "false"
		flusher, ok := w.(http.Flusher)
		if !ok {
			apiError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		// Subscribe BEFORE initial reads so no change between read and wait is missed.
		runWake, stopRun := a.changes.Subscribe(fleet.WorkflowRunTopic(id))
		defer stopRun()
		wfWake, stopWF := a.changes.Subscribe(fleet.WorkflowTopic(c.auth.WorkflowID))
		defer stopWF()
		keyWake, stopKey := a.changes.Subscribe(fleet.WorkflowKeyTopic(c.auth.KeyID))
		defer stopKey()

		row, found, err := readScopedRunRow(ctx, c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not read run")
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		transcriptStops := map[string]func(){}
		transcriptWake := make(chan struct{}, 1)
		defer func() {
			for _, stop := range transcriptStops {
				stop()
			}
		}()

		syncAgentSubs := func(host string, agents []agentRow) {
			if !includeTranscripts {
				return
			}
			for _, ag := range agents {
				if _, exists := transcriptStops[ag.ID]; exists {
					continue
				}
				ch, stop := a.changes.Subscribe(fleet.TranscriptTopic(host, ag.ID))
				done := make(chan struct{})
				go func() {
					for {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						case <-ch:
							select {
							case transcriptWake <- struct{}{}:
							default:
							}
						}
					}
				}()
				transcriptStops[ag.ID] = func() {
					close(done)
					stop()
				}
			}
		}

		flushTranscripts := func(rr runRow) error {
			if !includeTranscripts {
				return nil
			}
			agents, err := runAgents(ctx, c.pool, rr)
			if err != nil {
				return err
			}
			syncAgentSubs(rr.Host, agents)
			for _, ag := range agents {
				var epoch string
				err := c.pool.QueryRow(ctx, `SELECT epoch FROM transcripts WHERE host = $1 AND agent_id = $2`, rr.Host, ag.ID).Scan(&epoch)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				cur := cursor.agents[ag.ID]
				fromSeq := 0
				if cur != nil && cur.epoch == epoch {
					fromSeq = cur.seqStart
				} else {
					cur = nil
				}
				tr, err := c.reg.Transcript(ctx, rr.Host, ag.ID, fromSeq)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if cur == nil || cur.epoch != tr.Epoch {
					cur = &agentStreamPos{epoch: tr.Epoch}
					cursor.agents[ag.ID] = cur
				}
				for _, e := range tr.Entries {
					if e.SeqStart < cur.seqStart {
						continue
					}
					if e.SeqStart == cur.seqStart && e.SeqEnd <= cur.seqEnd && len(e.Text) == cur.textLen {
						continue
					}
					cur.seqStart = e.SeqStart
					cur.seqEnd = e.SeqEnd
					cur.textLen = len(e.Text)
					payload := struct {
						RunID   string `json:"runId"`
						AgentID string `json:"agentId"`
						Epoch   string `json:"epoch"`
						apiTranscriptEntry
					}{
						RunID:              id,
						AgentID:            ag.ID,
						Epoch:              tr.Epoch,
						apiTranscriptEntry: apiEntry(e),
					}
					if err := writeSSE(w, flusher, "transcript", cursor.encode(), payload); err != nil {
						return err
					}
				}
			}
			return nil
		}

		lastSummary := runSummaryFromRow(row)
		if err := writeSSE(w, flusher, "run", "", lastSummary); err != nil {
			return
		}
		events, _, err := readRunEventsWithID(ctx, c.pool, id, cursor.eventID)
		if err != nil {
			_ = writeSSE(w, flusher, "error", "", map[string]string{"error": "could not read events"})
			return
		}
		for _, ev := range events {
			cursor.eventID = ev.ID
			if err := writeSSE(w, flusher, "run_event", cursor.encode(), ev); err != nil {
				return
			}
		}
		if err := flushTranscripts(row); err != nil {
			return
		}

		grace := a.sseTerminalGrace
		if grace <= 0 {
			grace = defaultSSETerminalGrace
		}
		var graceTimer *time.Timer
		var graceCh <-chan time.Time
		startGrace := func() {
			if graceTimer != nil {
				return
			}
			graceTimer = time.NewTimer(grace)
			graceCh = graceTimer.C
		}
		defer func() {
			if graceTimer != nil {
				graceTimer.Stop()
			}
		}()

		if runTerminal(row.Status) {
			if !includeTranscripts || (row.FinishedAt != nil && a.cfg.Now().Sub(*row.FinishedAt) >= grace) {
				_ = writeSSE(w, flusher, "done", cursor.encode(), lastSummary)
				return
			}
			startGrace()
		}

		interval := a.sseKeepalive
		if interval <= 0 {
			interval = defaultSSEInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		checkKey := func() bool {
			if err := a.store.CheckWorkflowAPIKeyActive(ctx, c.auth.KeyID); err != nil {
				_ = writeSSE(w, flusher, "error", "", map[string]string{"error": err.Error()})
				return false
			}
			return true
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-keyWake:
				if !checkKey() {
					return
				}
			case <-wfWake:
				if !checkKey() {
					return
				}
			case <-graceCh:
				if !checkKey() {
					return
				}
				_ = flushTranscripts(row)
				_ = writeSSE(w, flusher, "done", cursor.encode(), lastSummary)
				return
			case <-ticker.C:
				if !checkKey() {
					return
				}
				if err := flushTranscripts(row); err != nil {
					return
				}
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-runWake:
				if !checkKey() {
					return
				}
				rr, ok, err := readScopedRunRow(ctx, c.pool, c.auth.WorkflowID, id)
				if err != nil || !ok {
					_ = writeSSE(w, flusher, "error", "", map[string]string{"error": "run no longer available"})
					return
				}
				row = rr
				evs, _, err := readRunEventsWithID(ctx, c.pool, id, cursor.eventID)
				if err != nil {
					return
				}
				for _, ev := range evs {
					cursor.eventID = ev.ID
					if err := writeSSE(w, flusher, "run_event", cursor.encode(), ev); err != nil {
						return
					}
				}
				curSummary := runSummaryFromRow(rr)
				if runSnapshotChanged(lastSummary, curSummary) {
					lastSummary = curSummary
					if err := writeSSE(w, flusher, "run", "", curSummary); err != nil {
						return
					}
				}
				if err := flushTranscripts(rr); err != nil {
					return
				}
				if runTerminal(rr.Status) {
					if !includeTranscripts {
						_ = writeSSE(w, flusher, "done", cursor.encode(), curSummary)
						return
					}
					startGrace()
				}
			case <-transcriptWake:
				if !checkKey() {
					return
				}
				if err := flushTranscripts(row); err != nil {
					return
				}
			}
		}
	}
}

// --- Control actions (all scoped to c.auth.WorkflowID) ---

// apiCancelRun: POST /api/workflow-api/runs/{id}/cancel {}.
func (a *app) apiCancelRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeAPIBody(w, r, 1024, &body) {
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		if err := a.wf.Cancel(c.actorCtx(r.Context()), c.auth.Account.ID, id); err != nil {
			apiWorkflowError(w, err)
			return
		}
		a.startQueued(r.Context())
		writeJSON(w, map[string]string{"id": id, "status": workflow.Canceled})
	}
}

// apiReplyToRun: POST /api/workflow-api/runs/{id}/reply {"text", "attachments"}.
func (a *app) apiReplyToRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text        string                   `json:"text"`
			Attachments []workflow.APIAttachment `json:"attachments,omitempty"`
		}
		if !decodeAPIBody(w, r, maxWorkflowAPIBody, &req) {
			return
		}
		if len(req.Text) > workflow.MaxAPITextBytes {
			apiError(w, http.StatusRequestEntityTooLarge, "text exceeds 1 MiB limit")
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		if err := a.wf.ReplyWithAttachments(c.actorCtx(r.Context()), c.auth.Account.ID, id, req.Text, req.Attachments); err != nil {
			apiWorkflowError(w, err)
			return
		}
		writeJSON(w, map[string]string{"id": id, "status": workflow.Running})
	}
}

// apiAddRunInput: POST /api/workflow-api/runs/{id}/input {"text", "attachments"}.
func (a *app) apiAddRunInput(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text        string                   `json:"text"`
			Attachments []workflow.APIAttachment `json:"attachments,omitempty"`
		}
		if !decodeAPIBody(w, r, maxWorkflowAPIBody, &req) {
			return
		}
		if len(req.Text) > workflow.MaxAPITextBytes {
			apiError(w, http.StatusRequestEntityTooLarge, "text exceeds 1 MiB limit")
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		if err := a.wf.AddInputWithAttachments(c.actorCtx(r.Context()), c.auth.Account.ID, id, req.Text, req.Attachments); err != nil {
			apiWorkflowError(w, err)
			return
		}
		writeJSON(w, map[string]string{"id": id, "status": workflow.Running})
	}
}

// apiRetryRun: POST /api/workflow-api/runs/{id}/retry {}.
func (a *app) apiRetryRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.auth.APITriggerEnabled {
			apiError(w, http.StatusForbidden, "API trigger is disabled for this workflow")
			return
		}
		var body struct{}
		if !decodeAPIBody(w, r, 1024, &body) {
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		newID, err := a.wf.RetryRun(c.actorCtx(r.Context()), c.auth.Account.ID, id)
		if err != nil {
			apiWorkflowError(w, err)
			return
		}
		row, found, err := readScopedRunRow(r.Context(), c.pool, c.auth.WorkflowID, newID)
		if err != nil || !found {
			apiError(w, http.StatusInternalServerError, "retry created but could not be read")
			return
		}
		w.Header().Set("Location", "/api/workflow-api/runs/"+newID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(runSummaryFromRow(row))
	}
}

// apiResumeRun: POST /api/workflow-api/runs/{id}/resume {}.
func (a *app) apiResumeRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeAPIBody(w, r, 1024, &body) {
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		if err := a.wf.Resume(c.actorCtx(r.Context()), c.auth.Account.ID, id); err != nil {
			apiWorkflowError(w, err)
			return
		}
		writeJSON(w, map[string]string{"id": id, "status": workflow.Running})
	}
}

// apiStartQueuedRun: POST /api/workflow-api/runs/{id}/start {}.
func (a *app) apiStartQueuedRun(c apiCaller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeAPIBody(w, r, 1024, &body) {
			return
		}
		id := r.PathValue("id")
		ok, err := verifyScopedRun(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not check run")
			return
		}
		if !ok {
			apiError(w, http.StatusNotFound, "no such run in this workflow")
			return
		}
		if err := a.wf.StartNow(c.actorCtx(r.Context()), c.auth.Account.ID, id); err != nil {
			apiWorkflowError(w, err)
			return
		}
		row, found, err := readScopedRunRow(r.Context(), c.pool, c.auth.WorkflowID, id)
		if err != nil || !found {
			writeJSON(w, map[string]string{"id": id, "status": workflow.Running})
			return
		}
		writeJSON(w, runSummaryFromRow(row))
	}
}
