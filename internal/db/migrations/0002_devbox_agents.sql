-- Every unarchived Paseo agent on each dev box, as of the box's latest report (paseo ls -g).
-- A report replaces its box's rows. A box whose agent list failed has none, and says why in
-- devboxes.agents_error.
CREATE TABLE devbox_agents (
    box      text NOT NULL REFERENCES devboxes (name) ON DELETE CASCADE,
    id       text NOT NULL,
    short_id text NOT NULL DEFAULT '',
    name     text NOT NULL DEFAULT '',
    provider text NOT NULL DEFAULT '', -- provider/model
    thinking text NOT NULL DEFAULT '',
    status   text NOT NULL DEFAULT '', -- running, idle, closed, error, ...
    cwd      text NOT NULL DEFAULT '',
    PRIMARY KEY (box, id)
);

-- Counts come from devbox_agents now, not from a copy kept on devboxes.
DROP VIEW devbox_status;
ALTER TABLE devboxes DROP COLUMN agents;
CREATE VIEW devbox_status AS
SELECT d.*,
       d.last_seen > now() - interval '1 minute' AS online,
       (SELECT count(*) FROM devbox_agents a WHERE a.box = d.name)::int AS agents,
       (SELECT count(*) FROM devbox_agents a WHERE a.box = d.name AND a.status = 'running')::int AS running_agents
FROM devboxes d;
