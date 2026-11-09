-- Absence during a daemon outage does not imply archival. Keep the last confirmed state.
ALTER TABLE workflow_run_agents ADD COLUMN archived boolean NOT NULL DEFAULT false;
CREATE INDEX workflow_run_agents_unarchived ON workflow_run_agents (host, agent_id) WHERE NOT archived;
UPDATE workflow_run_agents h SET archived = true
WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.host = h.host AND a.id = h.agent_id)
    AND (EXISTS (SELECT 1 FROM hosts b WHERE b.name = h.host AND b.connected)
        OR EXISTS (SELECT 1 FROM workflow_runs r JOIN commands c ON c.id = r.archive_command
            WHERE r.id = h.run_id AND c.status = 'succeeded' AND r.workspace_id = h.agent->>'workspace_id'));

-- A completed workspace archive is authoritative even before the next connected report.
-- Outside-workspace descendants rely on the daemon's confirmed absence instead.
CREATE VIEW archived_workflow_agents WITH (security_invoker = true) AS
SELECT DISTINCT h.host, h.agent_id
FROM workflow_run_agents h
WHERE h.archived OR EXISTS (
    SELECT 1 FROM workflow_runs r JOIN commands c ON c.id = r.archive_command
    WHERE r.id = h.run_id AND c.status = 'succeeded' AND r.workspace_id = h.agent->>'workspace_id'
);
