-- Cleanup has its own durable command, so a failure never changes a run's outcome.
-- Join commands for its progress, error, result and expiry through read-only SQL.
ALTER TABLE workflow_runs ADD COLUMN archive_command uuid REFERENCES commands ON DELETE SET NULL;

-- Keep the last reported agent and workspace for each run, independently of the live fleet.
-- JSON records retain every reported column; the views expose the same typed columns as the
-- live tables. Transcripts and diffs stay in their existing SQL-readable tables.
CREATE TABLE workflow_run_agents (
    run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
    host text NOT NULL REFERENCES hosts (name) ON DELETE CASCADE,
    agent_id text NOT NULL,
    agent jsonb NOT NULL,
    workspace jsonb NOT NULL,
    captured_at timestamptz NOT NULL,
    archived boolean NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, host, agent_id)
);
CREATE INDEX workflow_run_agents_host ON workflow_run_agents (host, agent_id);
ALTER TABLE workflow_run_agents ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_runs ON workflow_run_agents FOR SELECT
    USING (run_id IN (SELECT id FROM workflow_runs));

-- Live records take precedence. Absent agents are historical, never shown as running.
CREATE VIEW recorded_agents WITH (security_invoker = true) AS
SELECT a.* FROM agents a
UNION ALL
SELECT (jsonb_populate_record(NULL::agents,
    h.agent || '{"status":"closed","requires_attention":false,"attention_reason":""}'::jsonb)).*
FROM (SELECT DISTINCT ON (host, agent_id) * FROM workflow_run_agents
      ORDER BY host, agent_id, captured_at DESC, run_id) h
WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.host = h.host AND a.id = h.agent_id);

CREATE VIEW recorded_workspaces WITH (security_invoker = true) AS
SELECT w.* FROM workspaces w
UNION ALL
SELECT (jsonb_populate_record(NULL::workspaces,
    h.workspace || '{"listed":false,"status":"archived"}'::jsonb)).*
FROM (SELECT DISTINCT ON (host, workspace->>'id') * FROM workflow_run_agents
      ORDER BY host, workspace->>'id', captured_at DESC, run_id) h
WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.host = h.host AND w.id = h.workspace->>'id');

CREATE VIEW recorded_workspace_status WITH (security_invoker = true) AS
SELECT w.*,
       (SELECT count(*) FROM recorded_agents a WHERE a.host = w.host AND a.workspace_id = w.id)::int AS agents,
       (SELECT count(*) FROM recorded_agents a WHERE a.host = w.host AND a.workspace_id = w.id AND a.status = 'running')::int AS running_agents
FROM recorded_workspaces w;

-- Finished runs no longer participate in report-time history capture.
CREATE INDEX workflow_runs_capturing ON workflow_runs (host, id) WHERE finished_at IS NULL;

CREATE INDEX workflow_run_agents_unarchived ON workflow_run_agents (host, agent_id) WHERE NOT archived;

-- A completed workspace archive is authoritative even before the next connected report.
-- Outside-workspace descendants rely on the daemon's confirmed absence instead.
CREATE VIEW archived_workflow_agents WITH (security_invoker = true) AS
SELECT DISTINCT h.host, h.agent_id
FROM workflow_run_agents h
WHERE h.archived OR EXISTS (
    SELECT 1 FROM workflow_runs r JOIN commands c ON c.id = r.archive_command
    WHERE r.id = h.run_id AND c.status = 'succeeded' AND r.workspace_id = h.agent->>'workspace_id'
);
