-- Per-host plan authentication state: each host keeps its own cswap slot and auth source
-- (cswap, claude_cli, codex_auth, agy_cli, muse_cli), login fix commands adapt to whether
-- the host uses cswap, and plan_host_auth exposes every host's auth state for each plan.

ALTER TABLE usage_sources ADD COLUMN slot integer;
ALTER TABLE usage_sources ADD COLUMN auth_source text NOT NULL DEFAULT '';

-- Host-aware login fix functions: on a host that signs in directly with `claude auth login`
-- (auth_source = 'claude_cli', no cswap), suggest `claude auth login --claudeai` directly
-- instead of a cswap import sequence. Hosts with cswap (or legacy hosts with auth_source = '')
-- keep the safe throwaway CLAUDE_CONFIG_DIR + cswap import sequence.
CREATE FUNCTION login_fix_first_step(provider text, login text, auth_source text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN provider = 'claude' AND auth_source = 'claude_cli' THEN false
  ELSE login_fix_first_step(provider, login)
 END
$$;

CREATE FUNCTION login_fix_command(provider text, login text, auth_source text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN provider = 'claude' AND auth_source = 'claude_cli' AND position('@' IN login) > 0 THEN
   'claude auth login --claudeai --email ''' || replace(split_part(login, '#', 1), '''', '') || ''''
  WHEN provider = 'claude' AND auth_source = 'claude_cli' THEN
   'claude auth login --claudeai'
  ELSE login_fix_command(provider, login)
 END
$$;

CREATE FUNCTION login_fix(provider text, login text, auth_source text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN provider = 'claude' AND auth_source = 'claude_cli' AND position('@' IN login) > 0 THEN
   'Run `claude auth login --claudeai --email ' || split_part(login, '#', 1) || '` on the host and finish the sign-in it starts.'
  WHEN provider = 'claude' AND auth_source = 'claude_cli' THEN
   'Run `claude auth login --claudeai` on the host and finish the sign-in it starts.'
  ELSE login_fix(provider, login)
 END
$$;

-- Update login_alerts to use the host's auth_source for fix instructions and commands.
CREATE OR REPLACE VIEW login_alerts WITH (security_invoker = true) AS
SELECT 'broken'::text AS kind, i.id AS incident_id, i.host, i.provider, i.login,
       coalesce(nullif(a.label, ''), nullif(i.login, ''), i.provider) AS label,
       i.reason, i.message, login_fix(i.provider, i.login, coalesce(s.auth_source, '')) AS fix,
       login_fix_command(i.provider, i.login, coalesce(s.auth_source, '')) AS command,
       login_fix_first_step(i.provider, i.login, coalesce(s.auth_source, '')) AS first_step,
       i.source, i.opened_at AS since, s.login_expires_at AS expires_at,
       (SELECT count(*) FROM agent_interruptions x WHERE x.incident_id = i.id)::int AS interrupted,
       (SELECT count(*) FROM agent_interruptions x WHERE x.incident_id = i.id AND x.status = 'waiting')::int AS waiting,
       (SELECT count(*) FROM agent_interruptions x WHERE x.incident_id = i.id AND x.status IN ('resumed','parent_notified'))::int AS recovered,
       (SELECT count(*) FROM agent_interruptions x WHERE x.incident_id = i.id AND x.status IN ('gave_up','expired'))::int AS stuck
FROM login_incidents i
LEFT JOIN usage_accounts a ON a.provider = i.provider AND a.account = i.login
LEFT JOIN usage_sources s ON s.host = i.host AND s.provider = i.provider AND s.account = i.login
WHERE i.resolved_at IS NULL
UNION ALL
SELECT 'expiring', NULL, s.host, s.provider, s.account,
       coalesce(nullif(a.label, ''), s.account),
       'login_expiring', 'The sign-in ends ' || to_char(s.login_expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI') || ' UTC',
       'Renew it before then. ' || login_fix(s.provider, s.account, s.auth_source),
       login_fix_command(s.provider, s.account, s.auth_source),
       login_fix_first_step(s.provider, s.account, s.auth_source),
       'meter', s.observed_at, s.login_expires_at, 0, 0, 0, 0
FROM usage_sources s
LEFT JOIN usage_accounts a ON a.provider = s.provider AND a.account = s.account
WHERE s.account <> '' AND s.login_expires_at IS NOT NULL AND s.login_expires_at < now() + interval '5 days'
  AND NOT EXISTS (SELECT 1 FROM login_incidents i WHERE i.host = s.host AND i.provider = s.provider
                  AND i.login = s.account AND i.resolved_at IS NULL);

-- Per-host authentication and read state for every observed plan login.
CREATE VIEW plan_host_auth WITH (security_invoker = true) AS
WITH latest_active AS (
    SELECT DISTINCT ON (host, provider)
           host, provider, account, until
    FROM usage_active
    ORDER BY host, provider, since DESC
)
SELECT s.provider,
       s.account,
       coalesce(nullif(a.label, ''), s.account) AS label,
       s.host,
       coalesce(hs.online, false) AS host_online,
       (la.account = s.account AND la.until > now() - interval '15 minutes') IS TRUE AS active,
       s.slot,
       s.auth_source,
       s.ok,
       s.error,
       s.note,
       s.observed_at,
       s.received_at,
       s.login_expires_at,
       (s.received_at < now() - interval '15 minutes') AS stale,
       li.id AS incident_id,
       coalesce(li.reason, '') AS incident_reason,
       coalesce(li.message, '') AS incident_message,
       CASE
           WHEN li.id IS NOT NULL THEN 'broken'
           WHEN s.login_expires_at IS NOT NULL AND s.login_expires_at < now() + interval '5 days' THEN 'expiring'
           WHEN NOT s.ok AND s.received_at > now() - interval '15 minutes' THEN 'error'
           WHEN s.received_at < now() - interval '15 minutes' THEN 'stale'
           WHEN (la.account = s.account AND la.until > now() - interval '15 minutes') IS TRUE THEN 'active'
           ELSE 'standby'
       END AS status
FROM usage_sources s
LEFT JOIN usage_accounts a ON a.provider = s.provider AND a.account = s.account
LEFT JOIN host_status hs ON hs.name = s.host
LEFT JOIN latest_active la ON la.host = s.host AND la.provider = s.provider
LEFT JOIN LATERAL (
    SELECT i.id, i.reason, i.message
    FROM login_incidents i
    WHERE i.host = s.host AND i.provider = s.provider AND i.resolved_at IS NULL
      AND (i.login = s.account OR (i.login = '' AND (la.account = s.account AND la.until > now() - interval '15 minutes') IS TRUE))
    ORDER BY i.login <> '' DESC, i.opened_at ASC
    LIMIT 1
) li ON true
WHERE s.account <> '';
