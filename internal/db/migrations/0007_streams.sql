-- A box is online while it holds a stream to the server, not while it keeps re-reporting. Each
-- stream that attaches takes the box (stream_id); the server sees it on every ping (last_seen),
-- so a stream left behind by a server that died goes offline after a minute.
ALTER TABLE devboxes ADD COLUMN stream_id uuid; -- the box's current stream; null while it has none

COMMENT ON COLUMN devboxes.last_seen IS 'when the server last heard from the box: a report, or a ping on its stream';
COMMENT ON COLUMN devboxes.stream_id IS 'the box''s current stream to the server; null while it has none';

DROP VIEW devbox_status;
CREATE VIEW devbox_status AS
SELECT d.*,
       d.stream_id IS NOT NULL AND d.last_seen > now() - interval '1 minute' AS online,
       (SELECT count(*) FROM workspaces w WHERE w.box = d.name)::int AS workspaces,
       (SELECT count(*) FROM agents a WHERE a.box = d.name)::int AS agents,
       (SELECT count(*) FROM agents a WHERE a.box = d.name AND a.status = 'running')::int AS running_agents
FROM devboxes d;
