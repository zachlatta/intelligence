-- Grimm streaming, instant working state, live reasoning, per-session manager model,
-- pre-start warm-up filtering, and transcript_entries.received_at for server-side latency measurement.

ALTER TABLE grimm_sessions
    ADD COLUMN requested_manager_provider text NOT NULL DEFAULT '',
    ADD COLUMN requested_manager_model    text NOT NULL DEFAULT '',
    ADD COLUMN requested_manager_thinking text NOT NULL DEFAULT '';

ALTER TABLE grimm_managers
    ADD COLUMN prestarted boolean NOT NULL DEFAULT false;

ALTER TABLE transcript_entries
    ADD COLUMN received_at timestamptz NOT NULL DEFAULT now();

-- Include reasoning entries in the Grimm thread so the UI can stream live thinking, while
-- hiding pre-start warm-up turn chatter (before the human's first message / turn) from the thread.
CREATE OR REPLACE VIEW grimm_thread WITH (security_invoker = true) AS
SELECT g.session_id, 'm' || g.id AS item_id, g.created_at AS at, g.id AS message_id, NULL::integer AS epoch,
       NULL::integer AS seq, g.kind, g.sender, g.body, g.data, NULL::jsonb AS item, g.delivered_at
FROM grimm_messages g WHERE g.channel = 'thread'
UNION ALL
SELECT m.session_id, 't' || m.epoch || '.' || e.seq_start, coalesce(e.at, m.created_at), NULL, m.epoch, e.seq_start,
       e.kind, 'grimm', e.text, '{}'::jsonb, e.item, NULL
FROM grimm_managers m
JOIN transcript_entries e ON e.host = m.host AND e.agent_id = m.agent_id
LEFT JOIN LATERAL (
    SELECT min(hm.created_at) AS first_human_at
    FROM grimm_messages hm
    WHERE hm.session_id = m.session_id AND hm.kind = 'human'
) h ON m.prestarted
LEFT JOIN LATERAL (
    SELECT u.seq_start AS second_user_seq
    FROM transcript_entries u
    WHERE u.host = m.host AND u.agent_id = m.agent_id AND u.kind = 'user_message'
    ORDER BY u.seq_start ASC OFFSET 1 LIMIT 1
) u2 ON m.prestarted
WHERE m.agent_id <> ''
  AND e.kind IN ('assistant_message', 'tool_call', 'reasoning')
  AND (
    NOT m.prestarted
    OR (
      h.first_human_at IS NOT NULL
      AND (
        (u2.second_user_seq IS NOT NULL AND e.seq_start >= u2.second_user_seq)
        OR (u2.second_user_seq IS NULL AND coalesce(e.at, m.created_at) >= h.first_human_at)
      )
    )
  );

-- Report manager_state = 'starting' while create_agent is queued/running or a pre-started manager
-- is warming up before any human message has been sent, and 'working' immediately while a human
-- message is undelivered or a manager send_message is queued/sent/running.
DROP VIEW grimm_session_status;
CREATE VIEW grimm_session_status WITH (security_invoker = true) AS
SELECT s.*,
       coalesce(m.epoch, 0) AS manager_epoch, coalesce(m.agent_id, '') AS manager_agent_id,
       coalesce(m.status, '') AS manager_status, coalesce(m.error, '') AS manager_error,
       coalesce(nullif(m.provider, ''), s.requested_manager_provider) AS manager_provider,
       coalesce(nullif(m.model, ''), s.requested_manager_model) AS manager_model,
       coalesce(nullif(m.thinking, ''), s.requested_manager_thinking) AS manager_thinking,
       coalesce(m.login, '') AS manager_login,
       CASE
         WHEN s.workspace_error <> '' THEN 'offline'
         WHEN EXISTS (
           SELECT 1 FROM grimm_messages gm
           WHERE gm.session_id = s.id AND gm.kind = 'human' AND gm.delivered_at IS NULL
         ) AND (m.status IS NULL OR m.status <> 'failed') THEN 'working'
         WHEN m.status IS NULL OR m.status IN ('ended', 'failed') THEN 'offline'
         WHEN m.status IN ('pending', 'starting') THEN 'starting'
         WHEN EXISTS (
           SELECT 1 FROM commands c
           WHERE c.host = s.host AND c.status IN ('queued', 'sent', 'running')
             AND c.kind = 'create_agent' AND c.id = m.command_id
         ) THEN 'starting'
         WHEN EXISTS (
           SELECT 1 FROM commands c
           WHERE c.host = s.host AND c.status IN ('queued', 'sent', 'running')
             AND c.kind = 'send_message' AND m.agent_id <> '' AND c.args->>'agentId' = m.agent_id
         ) THEN 'working'
         WHEN a.status IS NULL THEN 'offline'
         WHEN a.status IN ('running', 'initializing')
              AND m.prestarted
              AND NOT EXISTS (SELECT 1 FROM grimm_messages gm WHERE gm.session_id = s.id AND gm.kind = 'human')
           THEN 'starting'
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
