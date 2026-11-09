-- A workspace's tags: where it came from and the like. 'Paseo GUI': made in the Paseo app (desktop,
-- web or mobile), as its creation receipt on the host says.
ALTER TABLE workspaces ADD COLUMN tags text[] NOT NULL DEFAULT '{}';
COMMENT ON COLUMN workspaces.tags IS 'the workspace''s tags, such as ''Paseo GUI'' (made in the Paseo app)';

-- workspace_status names w.* columns as they were when it was made: make it again to include tags.
DROP VIEW workspace_status;
CREATE VIEW workspace_status WITH (security_invoker = true) AS
SELECT w.*,
       (SELECT count(*) FROM agents a WHERE a.host = w.host AND a.workspace_id = w.id)::int AS agents,
       (SELECT count(*) FROM agents a WHERE a.host = w.host AND a.workspace_id = w.id AND a.status = 'running')::int AS running_agents
FROM workspaces w;
