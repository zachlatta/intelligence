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
