-- Logins that stopped working, and the agent turns they broke (internal/interrupt).
--
-- A Claude or Codex sign-in can end under the agents using it (Claude logins have a hard ~30-day
-- limit). Every agent on the host then fails its next turn with the provider's own message
-- ("Failed to authenticate: this login reached its time limit…") and sits idle as if it had
-- finished. The server recognises that message as transcripts arrive, records the interruption,
-- and marks the host's login broken until a fresh meter reading of it works again or someone marks
-- it fixed. A worker then resumes each interrupted agent once a working login is active.

-- One row per host login while it is broken. login is usage_accounts.account (the email for
-- Claude); '' when the host's login was not known.
CREATE TABLE login_incidents (
 id bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
 host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
 provider text NOT NULL,
 login text NOT NULL DEFAULT '',
 reason text NOT NULL CHECK (reason IN ('login_expired','logged_out','auth_rejected')),
 message text NOT NULL DEFAULT '',          -- what the provider (or the host's meter) said, one line
 source text NOT NULL CHECK (source IN ('transcript','meter')),
 opened_at timestamptz NOT NULL,
 last_seen_at timestamptz NOT NULL,
 resolved_at timestamptz,
 resolved_by text NOT NULL DEFAULT '' CHECK (resolved_by IN ('','reading','manual')),
 resolved_note text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX login_incidents_open ON login_incidents (host, provider, login) WHERE resolved_at IS NULL;

-- One row per agent turn a login failure stopped (the transcript entry that says so), and what
-- Intelligence did about it:
--   waiting          the agent waits for a working login
--   resumed          it was asked to carry on (the same agent: same model, its context intact)
--   parent_notified  its parent (a workflow orchestrator, say) was told to resume or replace it
--   superseded       something else got it going first (a newer turn in its transcript)
--   gone             the agent is no longer on its host
--   gave_up          it failed again after 3 recoveries in 6 hours: someone has to look
--   expired          no working login for 24 hours
CREATE TABLE agent_interruptions (
 host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
 agent_id text NOT NULL,
 seq integer NOT NULL,                      -- the transcript entry (seq_start)
 at timestamptz NOT NULL,                   -- when the turn failed
 provider text NOT NULL,
 model text NOT NULL DEFAULT '',
 title text NOT NULL DEFAULT '',
 parent_id text NOT NULL DEFAULT '',
 reason text NOT NULL CHECK (reason IN ('login_expired','logged_out','auth_rejected')),
 message text NOT NULL DEFAULT '',
 incident_id bigint REFERENCES login_incidents ON DELETE SET NULL,
 run_id uuid REFERENCES workflow_runs ON DELETE SET NULL,
 status text NOT NULL DEFAULT 'waiting'
   CHECK (status IN ('waiting','resumed','parent_notified','superseded','gone','gave_up','expired')),
 target_agent_id text NOT NULL DEFAULT '',  -- who was messaged: the agent, or its parent
 command_id uuid REFERENCES commands ON DELETE SET NULL,
 outcome text NOT NULL DEFAULT '',          -- what was done, in words
 detected_at timestamptz NOT NULL,
 acted_at timestamptz,
 PRIMARY KEY (host, agent_id, seq)
);
CREATE INDEX agent_interruptions_waiting ON agent_interruptions (detected_at) WHERE status = 'waiting';
CREATE INDEX agent_interruptions_incident ON agent_interruptions (incident_id);
CREATE INDEX agent_interruptions_run ON agent_interruptions (run_id) WHERE run_id IS NOT NULL;

-- When each login's sign-in ends, where the host's tool says (cswap's loginExpiresAt).
ALTER TABLE usage_sources ADD COLUMN login_expires_at timestamptz;

ALTER TABLE login_incidents ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON login_incidents FOR SELECT
 USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));
ALTER TABLE agent_interruptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON agent_interruptions FOR SELECT
 USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));

-- How to sign a login in again, in one place for every note (login_alerts). A Claude login cswap
-- manages is signed in inside a throwaway CLAUDE_CONFIG_DIR and loaded into cswap, as on
-- 2026-11-19: a bare `claude auth login` would sign in over the live login every agent on the host
-- uses. login_fix_command is what Copy command copies; login_fix_first_step says it is only the
-- first step (a Claude login whose cswap email is unknown).
CREATE FUNCTION login_fix_first_step(provider text, login text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT provider = 'claude' AND position('@' IN login) = 0
$$;

CREATE FUNCTION login_fix_command(provider text, login text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN provider = 'claude' AND position('@' IN login) > 0 THEN
   'T=$(mktemp -d) && mkdir -p "$T/cfg" "$T/data"'
   || ' && CLAUDE_CONFIG_DIR="$T/cfg" claude auth login --claudeai --email '''
   || replace(split_part(login, '#', 1), '''', '') || ''''
   || ' && CLAUDE_CONFIG_DIR="$T/cfg" XDG_DATA_HOME="$T/data" cswap add'
   || ' && CLAUDE_CONFIG_DIR="$T/cfg" XDG_DATA_HOME="$T/data" cswap export "$T/export.json"'
   || ' && cswap import "$T/export.json" --force && rm -rf "$T" && cswap list'
  WHEN provider = 'claude' THEN 'claude auth login --claudeai'
  WHEN provider = 'codex' THEN 'codex login'
  ELSE '' END
$$;

CREATE FUNCTION login_fix(provider text, login text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN provider = 'claude' AND position('@' IN login) > 0 THEN
   'Paste Copy command into a terminal on the host. It signs ' || split_part(login, '#', 1)
   || ' in inside a throwaway CLAUDE_CONFIG_DIR, so the live login the agents use is not touched,'
   || ' then loads it into cswap (add, export, import --force) and lists the logins.'
   || ' Open the link it prints, approve as ' || split_part(login, '#', 1) || ', and paste the code back.'
   || ' If this is the login the host uses now, finish with `cswap --switch-to <its slot number> --force`.'
  WHEN provider = 'claude' THEN
   'First step: run `claude auth login --claudeai` on the host and finish the sign-in it starts.'
   || ' If cswap manages the host''s logins, sign in inside a throwaway CLAUDE_CONFIG_DIR instead and'
   || ' `cswap import` the result, so the live login is not overwritten.'
  WHEN provider = 'codex' THEN 'Run `codex login` on the host and finish the sign-in it starts.'
  ELSE 'Sign the provider''s CLI on the host in again.' END
$$;

-- What the interface warns about on every page: each broken login, with how many agents it
-- stopped and where their recovery stands, and each login whose sign-in ends within 5 days.
CREATE VIEW login_alerts WITH (security_invoker = true) AS
SELECT 'broken'::text AS kind, i.id AS incident_id, i.host, i.provider, i.login,
       coalesce(nullif(a.label, ''), nullif(i.login, ''), i.provider) AS label,
       i.reason, i.message, login_fix(i.provider, i.login) AS fix,
       login_fix_command(i.provider, i.login) AS command, login_fix_first_step(i.provider, i.login) AS first_step,
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
       'Renew it before then. ' || login_fix(s.provider, s.account),
       login_fix_command(s.provider, s.account), login_fix_first_step(s.provider, s.account),
       'meter', s.observed_at, s.login_expires_at, 0, 0, 0, 0
FROM usage_sources s
LEFT JOIN usage_accounts a ON a.provider = s.provider AND a.account = s.account
WHERE s.account <> '' AND s.login_expires_at IS NOT NULL AND s.login_expires_at < now() + interval '5 days'
  AND NOT EXISTS (SELECT 1 FROM login_incidents i WHERE i.host = s.host AND i.provider = s.provider
                  AND i.login = s.account AND i.resolved_at IS NULL);

-- New launches avoid a broken login: a model group member on it is not suggested.
CREATE OR REPLACE VIEW model_group_candidates WITH (security_invoker = true) AS
WITH pooled AS (
 SELECT m.*, 2 * (SELECT count(*) FROM workflow_model_suggestions s
                  WHERE s.account_id=m.account_id AND s.provider=m.provider
                    AND s.login=m.login AND s.long_window=m.long_window
                    AND s.created_at > now() - interval '60 minutes') AS penalty
 FROM model_group_members m
), inputs AS (
 SELECT m.*, g.token, g.name AS group_name, g.revision, h.name AS host,
        c.remaining_pct, c.last_observed, c.resets_at,
        c.remaining_pct - 100 * extract(epoch FROM least(c.resets_at, coalesce(m.eligible_until,c.resets_at)) - now()) / nullif(c.window_seconds,0)
          - m.penalty AS score,
        CASE
          WHEN EXISTS (SELECT 1 FROM provider_plan_settings ps WHERE ps.provider=m.provider AND NOT ps.enabled) THEN 'Provider disabled'
          WHEN NOT m.enabled THEN 'Disabled'
          WHEN m.eligible_until <= now() THEN 'Plan ended'
          WHEN NOT EXISTS (SELECT 1 FROM jsonb_array_elements(h.providers) p
                           WHERE p->>'name'=m.provider AND p->>'available'='true') THEN 'Provider unavailable'
          WHEN NOT EXISTS (SELECT 1 FROM provider_models p WHERE p.host=h.name AND p.provider=m.provider
                           AND p.id=m.model AND ((cardinality(p.thinking)=0 AND m.thinking='') OR m.thinking=ANY(p.thinking))) THEN 'Model or thinking unavailable'
          WHEN EXISTS (SELECT 1 FROM login_incidents li WHERE li.host=h.name AND li.provider=m.provider
                       AND li.login IN (m.login, '') AND li.resolved_at IS NULL) THEN 'Login broken'
          WHEN NOT EXISTS (SELECT 1 FROM usage_active a WHERE a.host=h.name AND a.provider=m.provider
                           AND a.account=m.login AND a.until > now() - interval '15 minutes'
                           AND a.since=(SELECT max(a2.since) FROM usage_active a2 WHERE a2.host=a.host AND a2.provider=a.provider)) THEN 'Inactive login'
          WHEN c.remaining_pct IS NULL OR c.stale OR c.window_seconds IS NULL OR c.window_seconds<=0 OR c.resets_at IS NULL THEN 'Meter unknown or stale'
          WHEN c.remaining_pct <= 5 THEN 'Quota reserve'
          WHEN EXISTS (SELECT 1 FROM usage_current gate
                       WHERE gate.provider=m.provider AND gate.account=m.login
                         AND regexp_replace(gate.window_id,'[^/]+$','')=regexp_replace(m.long_window,'[^/]+$','')
                         AND gate.window_id<>m.long_window
                         AND (gate.window_seconds<c.window_seconds OR gate.window_seconds IS NULL)
                         AND (gate.remaining_pct IS NULL OR gate.stale OR gate.remaining_pct<=5)) THEN 'Short-window quota unavailable'
          ELSE '' END AS reason
 FROM pooled m JOIN model_groups g ON g.id=m.group_id AND g.account_id=m.account_id AND g.deleted_at IS NULL
 JOIN host_keys k ON k.account_id=m.account_id JOIN hosts h ON h.name=k.host
 LEFT JOIN usage_current c ON c.provider=m.provider AND c.account=m.login AND c.window_id=m.long_window
)
SELECT *, reason='' AND remaining_pct - penalty > 5 AS eligible
FROM inputs;
