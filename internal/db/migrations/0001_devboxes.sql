-- One row per dev box: its latest report. The box re-reports every 15 seconds.
CREATE TABLE devboxes (
    name           text PRIMARY KEY,
    client_version text NOT NULL DEFAULT '',
    collected_at   timestamptz,          -- when the box read its state
    last_seen      timestamptz NOT NULL, -- when the server received the report
    paseo_error    text NOT NULL DEFAULT '', -- paseo status could not be run or read
    daemon         text NOT NULL DEFAULT '', -- running, stopped, ...
    connected      text NOT NULL DEFAULT '', -- reachable, unreachable, ...
    paseo_version  text NOT NULL DEFAULT '',
    hostname       text NOT NULL DEFAULT '',
    listen         text NOT NULL DEFAULT '',
    pid            integer NOT NULL DEFAULT 0,
    started_at     timestamptz,
    note           text NOT NULL DEFAULT '',
    providers      jsonb NOT NULL DEFAULT '[]', -- [{"name","status","available"}]
    providers_at   timestamptz,
    agents         jsonb,                        -- {"running": 2, "idle": 8}; null when unknown
    agents_error   text NOT NULL DEFAULT ''
);

-- A box is online while it reported within the last minute.
CREATE VIEW devbox_status AS
SELECT *, last_seen > now() - interval '1 minute' AS online
FROM devboxes;
