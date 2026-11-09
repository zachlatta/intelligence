-- Each host's Paseo schedules: a prompt the daemon runs on a cadence, each time in a new agent
-- (or in an existing one). The host lists them on every status read and after each schedule
-- command, and sends them when they changed (hostclient.ScheduleReport): a report replaces its host's
-- schedules. Their runs are kept as they arrive, so the history outlives what the host sends.

-- When the host last listed its schedules, and why it could not (a daemon without schedules).
ALTER TABLE hosts
    ADD COLUMN schedules_at    timestamptz,
    ADD COLUMN schedules_error text NOT NULL DEFAULT '';

CREATE TABLE schedules (
    host              text NOT NULL REFERENCES hosts (name) ON DELETE CASCADE,
    id                text NOT NULL,
    name              text NOT NULL DEFAULT '',
    prompt            text NOT NULL,
    status            text NOT NULL DEFAULT '', -- active, paused, completed
    every_ms          bigint,                   -- cadence: every this long, or
    cron              text NOT NULL DEFAULT '', -- on this cron expression
    timezone          text NOT NULL DEFAULT '', -- in this zone ('' the daemon's)
    target            text NOT NULL DEFAULT '', -- new-agent (each run a new agent) or agent (an existing one)
    agent_id          text NOT NULL DEFAULT '', -- target agent
    provider          text NOT NULL DEFAULT '', -- the new agent's
    cwd               text NOT NULL DEFAULT '',
    model             text NOT NULL DEFAULT '',
    mode              text NOT NULL DEFAULT '',
    thinking          text NOT NULL DEFAULT '',
    isolation         text NOT NULL DEFAULT '', -- local (the directory) or worktree (a new git worktree each run)
    archive_on_finish boolean NOT NULL DEFAULT true,
    title             text NOT NULL DEFAULT '',
    max_runs          integer,                  -- null: no limit
    created_at        timestamptz,
    updated_at        timestamptz,
    next_run_at       timestamptz,
    last_run_at       timestamptz,
    paused_at         timestamptz,
    expires_at        timestamptz,
    received_at       timestamptz NOT NULL,
    PRIMARY KEY (host, id)
);
COMMENT ON TABLE schedules IS 'each host''s Paseo schedules, as the host last listed them';

-- Every run the host has sent. A run's output is the agent's last message (cut at 64 KB).
CREATE TABLE schedule_runs (
    host          text NOT NULL,
    schedule_id   text NOT NULL,
    id            text NOT NULL,
    status        text NOT NULL, -- running, succeeded, failed
    scheduled_for timestamptz,
    started_at    timestamptz,
    ended_at      timestamptz,
    agent_id      text NOT NULL DEFAULT '',
    workspace_id  text NOT NULL DEFAULT '',
    output        text NOT NULL DEFAULT '',
    error         text NOT NULL DEFAULT '',
    received_at   timestamptz NOT NULL,
    PRIMARY KEY (host, schedule_id, id),
    FOREIGN KEY (host, schedule_id) REFERENCES schedules (host, id) ON DELETE CASCADE
);
CREATE INDEX schedule_runs_started ON schedule_runs (host, schedule_id, started_at DESC);

-- Each schedule with its runs counted and its newest run.
CREATE VIEW schedule_status AS
SELECT s.*,
       (SELECT count(*) FROM schedule_runs r WHERE r.host = s.host AND r.schedule_id = s.id)::int AS runs,
       (SELECT count(*) FROM schedule_runs r WHERE r.host = s.host AND r.schedule_id = s.id AND r.status = 'failed')::int AS failed_runs,
       EXISTS (SELECT 1 FROM schedule_runs r WHERE r.host = s.host AND r.schedule_id = s.id AND r.status = 'running') AS running,
       l.id AS last_run_id, l.status AS last_run_status, l.started_at AS last_run_started_at, l.ended_at AS last_run_ended_at,
       l.agent_id AS last_run_agent_id, l.error AS last_run_error
FROM schedules s
LEFT JOIN LATERAL (SELECT r.id, r.status, r.started_at, r.ended_at, r.agent_id, r.error FROM schedule_runs r
                   WHERE r.host = s.host AND r.schedule_id = s.id ORDER BY r.started_at DESC NULLS LAST, r.id DESC LIMIT 1) l ON true;

ALTER TABLE schedules ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON schedules FOR SELECT
    USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));
ALTER TABLE schedule_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON schedule_runs FOR SELECT
    USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));
ALTER VIEW schedule_status SET (security_invoker = true);
