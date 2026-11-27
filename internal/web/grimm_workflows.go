package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/grimm"
)

//go:embed grimm_runs.js
var grimmRunsJS []byte

//go:embed grimm_runs.css
var grimmRunsCSS []byte

var (
	grimmRunsScript = asset("grimm_runs.js", "text/javascript; charset=utf-8", grimmRunsJS)
	grimmRunsStyle  = asset("grimm_runs.css", "text/css; charset=utf-8", grimmRunsCSS)
)

func (a *app) grimmWorkflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /grimm_runs.js", grimmRunsScript)
	mux.HandleFunc("GET /grimm_runs.css", grimmRunsStyle)
	mux.HandleFunc("POST /api/grimm/sessions/{id}/workflow-requests/{pid}/input", a.write(a.grimmWorkflowInput))
}

// grimmWorkflowInput is POST /api/grimm/sessions/{id}/workflow-requests/{pid}/input {"input": "..."}.
// Browser-session only (a.write): lets the human edit the task input of a pending Grimm workflow
// run proposal before clicking Approve.
func (a *app) grimmWorkflowInput(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, (1<<20)+4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			grimmError(w, &grimm.Error{Status: http.StatusBadRequest, Msg: "bad request: " + err.Error()})
			return
		}
		if err := a.grimm.UpdateWorkflowRequestInput(r.Context(), v.acct.ID, r.PathValue("id"), r.PathValue("pid"), req.Input); err != nil {
			grimmError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// notifyGrimmProposal wakes the Grimm session's SSE stream and supervisor if proposalID belongs
// to a Grimm session. Best-effort after a proposal decision commits.
func (a *app) notifyGrimmProposal(ctx context.Context, proposalID string) {
	var sessionID string
	if err := a.cfg.DB.QueryRow(ctx, `SELECT session_id::text FROM grimm_workflow_runs WHERE proposal_id = $1`, proposalID).Scan(&sessionID); err != nil {
		return
	}
	_, _ = a.cfg.DB.Exec(ctx, fleet.NotifySQL, fleet.GrimmTopic(sessionID))
	_, _ = a.cfg.DB.Exec(ctx, fleet.NotifySQL, fleet.GrimmTopic(""))
}

// grimmFollowWorkflowRuns subscribes to fleet.WorkflowRunTopic for each started run linked to
// session id so the Grimm SSE stream updates live as the workflow run progresses.
func (a *app) grimmFollowWorkflowRuns(ctx context.Context, p *pgxpool.Pool, sessionID string, subs map[string]func(), out chan struct{}) error {
	rows, err := p.Query(ctx, `SELECT run_id::text FROM grimm_workflow_run_status WHERE session_id = $1 AND run_id IS NOT NULL`, sessionID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, rid := range ids {
		if subs[rid] != nil {
			continue
		}
		ch, stop := a.changes.Subscribe(fleet.WorkflowRunTopic(rid))
		done := make(chan struct{})
		go relay(ctx, done, ch, out)
		subs[rid] = func() { close(done); stop() }
	}
	return nil
}

// grimmSendWorkflowRuns reads the session's grimm_workflow_run_status rows through the viewer's
// read-only pool and pushes a `workflow_runs` SSE event when anything changed.
func grimmSendWorkflowRuns(ctx context.Context, p *pgxpool.Pool, w http.ResponseWriter, flusher http.Flusher, sessionID string, last *string) error {
	items, err := grimmJSON(ctx, p, `SELECT to_jsonb(r) FROM grimm_workflow_run_status r WHERE r.session_id = $1 ORDER BY r.created_at`, sessionID)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	if string(b) == *last {
		return nil
	}
	*last = string(b)
	return writeSSE(w, flusher, "workflow_runs", "", json.RawMessage(b))
}
