-- Count each workspace's recorded agents in one grouped pass. The correlated counts re-read and
-- de-duplicated all workflow history once per workspace, twice, which passed the read roles'
-- statement timeout as history grew. Same columns, in the same order.
CREATE OR REPLACE VIEW recorded_workspace_status WITH (security_invoker = true) AS
SELECT w.*,
       coalesce(c.agents, 0)::int AS agents,
       coalesce(c.running_agents, 0)::int AS running_agents
FROM recorded_workspaces w
LEFT JOIN (SELECT host, workspace_id, count(*) AS agents,
                  count(*) FILTER (WHERE status = 'running') AS running_agents
           FROM recorded_agents GROUP BY host, workspace_id) c
       ON c.host = w.host AND c.workspace_id = w.id;
