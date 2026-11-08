-- Commands the server sends dev boxes: create, rename and archive workspaces and agents, send
-- agents messages. Each waits here until its box's stream is up, goes down it (sent), and the box
-- reports it running, then succeeded or failed. One not finished by expires_at is expired: never
-- delivered, or delivered with no result (it may have run).
CREATE TABLE commands (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    box         text NOT NULL REFERENCES devboxes (name) ON DELETE CASCADE,
    kind        text NOT NULL, -- create_workspace, create_agent, archive_agent, send_message, ...
    args        jsonb NOT NULL DEFAULT '{}',
    status      text NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'sent', 'running', 'succeeded', 'failed', 'expired')),
    result      jsonb,                     -- what it made or changed: {"workspaceId", "agentId"}
    error       text NOT NULL DEFAULT '',
    attempts    integer NOT NULL DEFAULT 0, -- times sent down a stream
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    sent_at     timestamptz,               -- first sent
    started_at  timestamptz,
    finished_at timestamptz
);

CREATE INDEX commands_open ON commands (box, created_at) WHERE status IN ('queued', 'sent', 'running');
CREATE INDEX commands_box ON commands (box, created_at DESC);
