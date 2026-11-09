-- Older finished runs have no snapshots. Recover the metadata still present in the live
-- fleet, without changing membership for any run that already has history. Known creation
-- times exclude work started after completion; older agents may have no creation timestamp.
WITH RECURSIVE candidates AS MATERIALIZED (
    SELECT r.id, r.host, r.workspace_id, r.orchestrator_id, r.project_agent_id, r.finished_at
    FROM workflow_runs r
    WHERE NOT EXISTS (SELECT 1 FROM workflow_run_agents h WHERE h.run_id = r.id)
), nodes AS MATERIALIZED (
    SELECT c.id AS run_id, a.host, a.id, a.parent_id, a.workspace_id
    FROM candidates c JOIN agents a ON a.host = c.host
    WHERE c.finished_at IS NULL OR a.created_at IS NULL OR a.created_at <= c.finished_at
        OR a.id IN (c.orchestrator_id, c.project_agent_id)
), tree AS (
    SELECT c.id AS run_id, n.host, n.id AS agent_id
    FROM candidates c JOIN nodes n ON n.run_id = c.id
        AND (n.id IN (c.orchestrator_id, c.project_agent_id) OR n.workspace_id = NULLIF(c.workspace_id, ''))
    UNION
    SELECT t.run_id, n.host, n.id FROM tree t
    JOIN nodes n ON n.run_id = t.run_id AND n.parent_id = t.agent_id
)
INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
SELECT t.run_id, a.host, a.id, to_jsonb(a), to_jsonb(w), now()
FROM tree t JOIN agents a ON a.host = t.host AND a.id = t.agent_id
JOIN workspaces w ON w.host = a.host AND w.id = a.workspace_id
ON CONFLICT DO NOTHING;
