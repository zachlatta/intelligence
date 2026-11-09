package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// CaptureWorkflowAgents remembers only active runs with a listed workspace (or live finder).
// Reports call it before replacing live rows; terminal moves take one final snapshot before
// setting finished_at. runID limits a terminal move to its own run; reports pass "".
// Shared run locks serialize capture with terminal moves, so membership freezes at completion.
func CaptureWorkflowAgents(ctx context.Context, tx pgx.Tx, host, runID string, at time.Time) error {
	_, err := tx.Exec(ctx, `WITH RECURSIVE candidates AS MATERIALIZED (
  SELECT r.id, r.host, r.workspace_id, r.orchestrator_id, r.project_agent_id
  FROM workflow_runs r
  WHERE r.host = $1 AND r.finished_at IS NULL AND ($3 = '' OR r.id::text = $3)
    AND (EXISTS (SELECT 1 FROM workspaces w WHERE w.host=r.host AND w.id=r.workspace_id AND w.listed)
      OR EXISTS (SELECT 1 FROM agents a JOIN workspaces w ON w.host=a.host AND w.id=a.workspace_id
        WHERE a.host=r.host AND a.id=r.project_agent_id AND w.listed))
  ORDER BY r.id FOR SHARE OF r
 ), nodes AS MATERIALIZED (
  SELECT c.id AS run_id, a.host, a.id, a.parent_id, a.workspace_id
  FROM candidates c JOIN agents a ON a.host=c.host
  UNION ALL
  SELECT h.run_id,h.host,h.agent_id,h.agent->>'parent_id',h.agent->>'workspace_id'
  FROM candidates c JOIN workflow_run_agents h ON h.run_id=c.id
  WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.host=h.host AND a.id=h.agent_id)
 ), tree AS (
  SELECT c.id AS run_id,n.host,n.id AS agent_id
  FROM candidates c JOIN nodes n ON n.run_id=c.id
    AND (n.id=c.orchestrator_id OR n.id=c.project_agent_id OR n.workspace_id=NULLIF(c.workspace_id,''))
  UNION
  SELECT t.run_id,n.host,n.id FROM tree t JOIN nodes n ON n.run_id=t.run_id AND n.parent_id=t.agent_id
 )
 INSERT INTO workflow_run_agents (run_id,host,agent_id,agent,workspace,captured_at)
 SELECT t.run_id,a.host,a.id,to_jsonb(a),to_jsonb(w),$2
 FROM tree t JOIN agents a ON a.host=t.host AND a.id=t.agent_id
 JOIN workspaces w ON w.host=a.host AND w.id=a.workspace_id
 ON CONFLICT (run_id,host,agent_id) DO UPDATE SET
   agent=EXCLUDED.agent,workspace=EXCLUDED.workspace,captured_at=EXCLUDED.captured_at`, host, at, runID)
	return err
}
