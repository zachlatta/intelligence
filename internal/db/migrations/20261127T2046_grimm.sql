-- Grimm: a multi-agent coding session. One manager agent (Claude Opus 5.5) is the main thread;
-- it hires named workers on the account's plans through the model-group balancer, and they talk
-- on a shared bus. Every agent is a Paseo agent on a host, started and steered through the host
-- command queue (commands). All of a session's state is here, so a restart loses nothing.

CREATE TABLE grimm_sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    host         text NOT NULL,
    repo         text NOT NULL,              -- the repository directory on the host
    title        text NOT NULL DEFAULT '',
    branch       text NOT NULL,              -- the session worktree's new branch
    base         text NOT NULL DEFAULT '',   -- what it branches from ('' = Paseo's default)
    workspace_id text NOT NULL DEFAULT '',   -- the session worktree, once the host made it
    workspace_command_id uuid,               -- the create_workspace command that makes it
    workspace_error text NOT NULL DEFAULT '',
    max_workers  integer NOT NULL DEFAULT 20 CHECK (max_workers BETWEEN 1 AND 100),
    status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz
);
CREATE INDEX grimm_sessions_account ON grimm_sessions (account_id, created_at DESC);
CREATE INDEX grimm_sessions_active ON grimm_sessions (host) WHERE status = 'active';

-- Each manager agent a session has had, in order (epoch): /new starts the next.
CREATE TABLE grimm_managers (
    session_id  uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id  bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    epoch       integer NOT NULL,
    host        text NOT NULL,
    agent_id    text NOT NULL DEFAULT '',
    command_id  uuid,                        -- the create_agent command that starts it
    provider    text NOT NULL DEFAULT '',
    model       text NOT NULL DEFAULT '',
    thinking    text NOT NULL DEFAULT '',
    login       text NOT NULL DEFAULT '',
    long_window text NOT NULL DEFAULT '',
    score       double precision,
    -- pending: waiting for the workspace; starting: create_agent queued; live: running in Paseo;
    -- failed: could not start (error says why); ended: replaced by /new or the session archived.
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'starting', 'live', 'failed', 'ended')),
    error       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz,
    PRIMARY KEY (session_id, epoch)
);
CREATE INDEX grimm_managers_agent ON grimm_managers (host, agent_id) WHERE agent_id <> '';

CREATE TABLE grimm_workers (
    session_id    uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id    bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    role          text NOT NULL DEFAULT 'worker',
    provider      text NOT NULL,
    model         text NOT NULL,
    thinking      text NOT NULL DEFAULT '',
    mode          text NOT NULL DEFAULT '',
    login         text NOT NULL DEFAULT '',
    long_window   text NOT NULL DEFAULT '',
    score         double precision,
    remaining_pct double precision,
    host          text NOT NULL,
    agent_id      text NOT NULL DEFAULT '',
    command_id    uuid,
    -- starting: create_agent queued; running: on a turn; idle: its turn ended (its reply is
    -- last_response); failed: could not start; archived: done, its agent archived.
    status        text NOT NULL DEFAULT 'starting' CHECK (status IN ('starting', 'running', 'idle', 'failed', 'archived')),
    seen_running  boolean NOT NULL DEFAULT false, -- its agent was seen running since its last turn began
    result_posted boolean NOT NULL DEFAULT false, -- it posted a result to grimm this turn: its end is not news
    task          text NOT NULL DEFAULT '',
    last_response text NOT NULL DEFAULT '',
    directive_seq integer NOT NULL DEFAULT 0,   -- directives up to this one were in its first prompt
    turn_at       timestamptz NOT NULL DEFAULT now(), -- when its current turn was asked for
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    archived_at   timestamptz,
    PRIMARY KEY (session_id, name)
);
CREATE INDEX grimm_workers_agent ON grimm_workers (host, agent_id) WHERE agent_id <> '';

-- The main thread (what Zach wrote, updates, command output, notices) and the bus. The manager's
-- own words and tool calls are its transcript (transcript_entries), merged in by grimm_thread.
CREATE TABLE grimm_messages (
    id           bigserial PRIMARY KEY,
    session_id   uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    channel      text NOT NULL CHECK (channel IN ('thread', 'bus')),
    sender       text NOT NULL DEFAULT '',
    recipient    text NOT NULL DEFAULT '',
    -- thread: human, update, command, notice, divider; bus: chat, handoff, result, system, directive
    kind         text NOT NULL,
    body         text NOT NULL DEFAULT '',
    data         jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,                -- a human message: when it went to the manager
    command_id   uuid                        -- ... in which send_message
);
CREATE INDEX grimm_messages_session ON grimm_messages (session_id, channel, id);
CREATE INDEX grimm_messages_undelivered ON grimm_messages (session_id, id) WHERE kind = 'human' AND delivered_at IS NULL;

CREATE TABLE grimm_directives (
    session_id uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    seq        integer NOT NULL,
    text       text NOT NULL,
    added_by   text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz,
    PRIMARY KEY (session_id, seq)
);

-- A directive reaches each worker once: pushed at once to a busy one, held for an idle one until
-- its next message. The key makes "once" hold.
CREATE TABLE grimm_directive_deliveries (
    session_id   uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    worker       text NOT NULL,
    seq          integer NOT NULL,
    delivered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, worker, seq)
);

-- News for the manager (a worker finished, failed or wrote to it), batched into one
-- "[Grimm update]" message when it is idle.
CREATE TABLE grimm_wakeups (
    id           bigserial PRIMARY KEY,
    session_id   uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    worker       text NOT NULL DEFAULT '',
    summary      text NOT NULL,
    detail       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    dropped      boolean NOT NULL DEFAULT false, -- its worker was archived before it went
    message_id   bigint                          -- the thread's update row that carried it
);
CREATE INDEX grimm_wakeups_pending ON grimm_wakeups (session_id, id) WHERE delivered_at IS NULL;

-- What happened, in order: the activity feed and audit (hires, messages, stops, archives,
-- directive changes, manager starts), with who did it.
CREATE TABLE grimm_events (
    id         bigserial PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    at         timestamptz NOT NULL DEFAULT now(),
    actor      text NOT NULL DEFAULT '',
    event      text NOT NULL,
    level      text NOT NULL DEFAULT 'info' CHECK (level IN ('info', 'warn')),
    text       text NOT NULL DEFAULT '',
    data       jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX grimm_events_session ON grimm_events (session_id, id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['grimm_sessions', 'grimm_managers', 'grimm_workers', 'grimm_messages',
                             'grimm_directives', 'grimm_directive_deliveries', 'grimm_wakeups', 'grimm_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY own_rows ON %I FOR SELECT USING (account_id = current_account_id())', t);
    END LOOP;
END $$;

-- Every agent of a session: its managers and its workers. A host keeps these agents'
-- transcripts after Paseo archives them, so the thread and a worker's log outlive the agent.
CREATE VIEW grimm_agents WITH (security_invoker = true) AS
SELECT m.session_id, m.account_id, m.host, m.agent_id, 'manager'::text AS role, 'grimm'::text AS name, m.epoch
FROM grimm_managers m WHERE m.agent_id <> ''
UNION ALL
SELECT w.session_id, w.account_id, w.host, w.agent_id, 'worker', w.name, NULL::integer
FROM grimm_workers w WHERE w.agent_id <> '';

-- Each worker as the Team panel shows it: its row, its agent's live status, and its newest
-- transcript entry (what it is doing now).
CREATE VIEW grimm_worker_status WITH (security_invoker = true) AS
SELECT w.*, coalesce(a.status, '') AS agent_status, coalesce(a.requires_attention, false) AS requires_attention,
       coalesce(a.attention_reason, '') AS attention_reason, last.item AS activity, last.at AS activity_at
FROM grimm_workers w
LEFT JOIN agents a ON a.host = w.host AND a.id = w.agent_id AND w.agent_id <> ''
LEFT JOIN LATERAL (
    SELECT e.item, e.at FROM transcript_entries e
    WHERE w.agent_id <> '' AND e.host = w.host AND e.agent_id = w.agent_id AND e.kind IN ('tool_call', 'assistant_message', 'reasoning')
    ORDER BY e.seq_start DESC LIMIT 1
) last ON true;

-- Each session with its current manager: whether it is offline (none yet, or ended), starting,
-- working or idle, and its counts.
CREATE VIEW grimm_session_status WITH (security_invoker = true) AS
SELECT s.*,
       coalesce(m.epoch, 0) AS manager_epoch, coalesce(m.agent_id, '') AS manager_agent_id,
       coalesce(m.status, '') AS manager_status, coalesce(m.error, '') AS manager_error,
       coalesce(m.provider, '') AS manager_provider, coalesce(m.model, '') AS manager_model,
       coalesce(m.login, '') AS manager_login,
       CASE
         WHEN m.status IS NULL OR m.status IN ('ended', 'failed') THEN 'offline'
         WHEN m.status IN ('pending', 'starting') THEN 'starting'
         WHEN a.status IS NULL THEN 'offline'
         WHEN a.status IN ('running', 'initializing') THEN 'working'
         ELSE 'idle'
       END AS manager_state,
       CASE WHEN a.status = 'running' THEN a.updated_at END AS manager_turn_at,
       (SELECT count(*) FROM grimm_workers w WHERE w.session_id = s.id AND w.status IN ('starting', 'running', 'idle'))::int AS live_workers,
       (SELECT count(*) FROM grimm_workers w WHERE w.session_id = s.id AND w.status IN ('starting', 'running'))::int AS working_workers,
       (SELECT count(*) FROM grimm_wakeups u WHERE u.session_id = s.id AND u.delivered_at IS NULL)::int AS pending_updates,
       h.online AS host_online
FROM grimm_sessions s
LEFT JOIN LATERAL (SELECT * FROM grimm_managers m WHERE m.session_id = s.id ORDER BY m.epoch DESC LIMIT 1) m ON true
LEFT JOIN agents a ON a.host = m.host AND a.id = m.agent_id AND m.agent_id <> ''
LEFT JOIN host_status h ON h.name = s.host;

-- The main thread in order: the thread's own rows, and every manager's words and tool calls from
-- its transcript (its user messages are the human and update rows already here).
CREATE VIEW grimm_thread WITH (security_invoker = true) AS
SELECT g.session_id, 'm' || g.id AS item_id, g.created_at AS at, g.id AS message_id, NULL::integer AS epoch,
       NULL::integer AS seq, g.kind, g.sender, g.body, g.data, NULL::jsonb AS item, g.delivered_at
FROM grimm_messages g WHERE g.channel = 'thread'
UNION ALL
SELECT m.session_id, 't' || m.epoch || '.' || e.seq_start, coalesce(e.at, m.created_at), NULL, m.epoch, e.seq_start,
       e.kind, 'grimm', e.text, '{}'::jsonb, e.item, NULL
FROM grimm_managers m
JOIN transcript_entries e ON e.host = m.host AND e.agent_id = m.agent_id
WHERE m.agent_id <> '' AND e.kind IN ('assistant_message', 'tool_call');

-- The balancer's view of a session's host: each distinct plan entry the account's model groups
-- offer there (model_group_candidates), with Grimm's spread penalty for the session's live
-- workers on the same login and window (15 points each), so hires spread across plans.
CREATE VIEW grimm_quota WITH (security_invoker = true) AS
SELECT s.id AS session_id, c.provider, c.model, c.thinking, c.login, c.long_window, c.remaining_pct, c.resets_at,
       c.last_observed, c.score AS raw_score,
       100 * extract(epoch FROM greatest(c.resets_at - now(), interval '0')) / nullif(u.window_seconds, 0) AS expected_remaining_pct,
       live.n AS live_workers, 15.0 * live.n AS spread_penalty, c.score - 15.0 * live.n AS score,
       c.eligible, c.reason, c.needs_switch
FROM grimm_sessions s
CROSS JOIN LATERAL (
    SELECT DISTINCT ON (c.provider, c.model, c.thinking, c.login, c.long_window) c.*
    FROM model_group_candidates c
    WHERE c.account_id = s.account_id AND c.host = s.host
    ORDER BY c.provider, c.model, c.thinking, c.login, c.long_window, c.eligible DESC, c.score DESC NULLS LAST
) c
LEFT JOIN usage_current u ON u.provider = c.provider AND u.account = c.login AND u.window_id = c.long_window
CROSS JOIN LATERAL (
    SELECT count(*)::int AS n FROM grimm_workers w
    WHERE w.session_id = s.id AND w.status IN ('starting', 'running', 'idle')
      AND w.provider = c.provider AND w.login = c.login AND w.long_window = c.long_window
) live;
