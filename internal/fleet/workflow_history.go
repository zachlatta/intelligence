package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// captureWorkflowAgents runs before replacing the live fleet and after storing the new report.
// Remember agents throughout a run, including descendants outside its workspace and agents
// archived before the last step. Previously captured parents keep their descendants connected
// to the run. Live rows are copied again so the last report wins; history never replaces them.
func captureWorkflowAgents(ctx context.Context, tx pgx.Tx, host string, at time.Time) error {
	_, err := tx.Exec(ctx, `WITH RECURSIVE tree AS (
  SELECT r.id AS run_id, a.host, a.id AS agent_id
  FROM workflow_runs r JOIN recorded_agents a ON a.host = r.host
    AND (a.id = r.orchestrator_id OR a.id = r.project_agent_id OR a.workspace_id = NULLIF(r.workspace_id, ''))
  WHERE r.host = $1
  UNION
  SELECT t.run_id, a.host, a.id FROM tree t
  JOIN recorded_agents a ON a.host = t.host AND a.parent_id = t.agent_id
 )
 INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
 SELECT t.run_id, a.host, a.id, to_jsonb(a), to_jsonb(w), $2
 FROM tree t JOIN agents a ON a.host = t.host AND a.id = t.agent_id
 JOIN workspaces w ON w.host = a.host AND w.id = a.workspace_id
 ON CONFLICT (run_id, host, agent_id) DO UPDATE SET
   agent = EXCLUDED.agent, workspace = EXCLUDED.workspace, captured_at = EXCLUDED.captured_at`, host, at)
	return err
}
