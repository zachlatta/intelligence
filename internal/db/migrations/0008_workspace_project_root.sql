-- A workspace's repository: its main checkout, from which a new worktree can be made.
ALTER TABLE workspaces ADD COLUMN project_root text NOT NULL DEFAULT '';

-- workspace_status names w.* columns as they were when it was made: make it again to include project_root.
DROP VIEW workspace_status;
CREATE VIEW workspace_status AS
SELECT w.*,
       (SELECT count(*) FROM agents a WHERE a.box = w.box AND a.workspace_id = w.id)::int AS agents,
       (SELECT count(*) FROM agents a WHERE a.box = w.box AND a.workspace_id = w.id AND a.status = 'running')::int AS running_agents
FROM workspaces w;
