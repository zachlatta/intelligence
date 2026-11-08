-- The hierarchy is devboxes > workspaces > agents. A dev box reads its Paseo daemon over the
-- daemon's own protocol, so its row keeps whether it is connected instead of the CLI's words.
DROP VIEW devbox_status;
DROP TABLE devbox_agents;
ALTER TABLE devboxes
    DROP COLUMN daemon,
    DROP COLUMN connected,
    DROP COLUMN note,
    DROP COLUMN agents_error,
    ADD COLUMN connected boolean NOT NULL DEFAULT false, -- the box holds a connection to its daemon
    ADD COLUMN status_error text NOT NULL DEFAULT '';    -- the last status read failed; status columns are older

COMMENT ON COLUMN devboxes.paseo_error IS 'why the box is not connected to its daemon';

-- A directory, local checkout or git worktree on a box that agents run in. A report replaces its
-- box's workspaces. listed = false: an agent's workspace that Paseo did not list (archived or
-- background), kept so the agent is not lost.
CREATE TABLE workspaces (
    box                  text NOT NULL REFERENCES devboxes (name) ON DELETE CASCADE,
    id                   text NOT NULL,
    listed               boolean NOT NULL DEFAULT true,
    name                 text NOT NULL DEFAULT '',
    kind                 text NOT NULL DEFAULT '', -- directory, local_checkout, worktree
    status               text NOT NULL DEFAULT '', -- running, attention, done
    directory            text NOT NULL DEFAULT '',
    project              text NOT NULL DEFAULT '',
    branch               text NOT NULL DEFAULT '',
    dirty                boolean NOT NULL DEFAULT false,
    additions            integer NOT NULL DEFAULT 0,
    deletions            integer NOT NULL DEFAULT 0,
    created_at           timestamptz,
    activity_at          timestamptz,
    last_user_message_at timestamptz,
    PRIMARY KEY (box, id)
);

-- Every unarchived Paseo agent, in its workspace.
CREATE TABLE agents (
    box                  text NOT NULL,
    id                   text NOT NULL,
    workspace_id         text NOT NULL,
    parent_id            text NOT NULL DEFAULT '', -- the agent that started it, for sub-agents
    title                text NOT NULL DEFAULT '',
    provider             text NOT NULL DEFAULT '',
    model                text NOT NULL DEFAULT '',
    thinking             text NOT NULL DEFAULT '',
    status               text NOT NULL DEFAULT '', -- running, idle, closed, error, ...
    cwd                  text NOT NULL DEFAULT '',
    requires_attention   boolean NOT NULL DEFAULT false,
    attention_reason     text NOT NULL DEFAULT '',
    created_at           timestamptz,
    updated_at           timestamptz,
    last_user_message_at timestamptz,
    PRIMARY KEY (box, id),
    FOREIGN KEY (box, workspace_id) REFERENCES workspaces (box, id) ON DELETE CASCADE
);

CREATE VIEW devbox_status AS
SELECT d.*,
       d.last_seen > now() - interval '1 minute' AS online,
       (SELECT count(*) FROM workspaces w WHERE w.box = d.name)::int AS workspaces,
       (SELECT count(*) FROM agents a WHERE a.box = d.name)::int AS agents,
       (SELECT count(*) FROM agents a WHERE a.box = d.name AND a.status = 'running')::int AS running_agents
FROM devboxes d;

CREATE VIEW workspace_status AS
SELECT w.*,
       (SELECT count(*) FROM agents a WHERE a.box = w.box AND a.workspace_id = w.id)::int AS agents,
       (SELECT count(*) FROM agents a WHERE a.box = w.box AND a.workspace_id = w.id AND a.status = 'running')::int AS running_agents
FROM workspaces w;
