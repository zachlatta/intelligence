-- User-maintained model groups. Token keys survive renames and are never reused.
CREATE TABLE model_groups (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    name text NOT NULL,
    token text NOT NULL CHECK (token ~ '^[a-z][a-z0-9_]*_model$'),
    revision integer NOT NULL DEFAULT 1,
    deleted_at timestamptz,
    UNIQUE (account_id, token), UNIQUE (account_id, id)
);
CREATE TABLE model_group_members (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    group_id uuid NOT NULL,
    position integer NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    thinking text NOT NULL,
    usage_provider text NOT NULL,
    login text NOT NULL,
    long_window text NOT NULL,
    gate_windows text[] NOT NULL DEFAULT '{}',
    eligible_until timestamptz,
    enabled boolean NOT NULL DEFAULT true,
    FOREIGN KEY (account_id, group_id) REFERENCES model_groups (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (usage_provider, login) REFERENCES usage_accounts (provider, account),
    UNIQUE (group_id, position)
);
CREATE TABLE workflow_model_suggestions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
    step integer NOT NULL,
    group_id uuid NOT NULL REFERENCES model_groups,
    token text NOT NULL,
    revision integer NOT NULL,
    usage_provider text,
    login text,
    long_window text,
    snapshot jsonb NOT NULL,
    message text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX model_suggestions_pool ON workflow_model_suggestions(account_id, usage_provider, login, long_window, created_at);
DO $$ DECLARE t text; BEGIN
    FOREACH t IN ARRAY ARRAY['model_groups','model_group_members','workflow_model_suggestions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY own_rows ON %I FOR SELECT USING (account_id = current_account_id())', t);
    END LOOP;
END $$;

-- One row per member and owned host. Scores and exclusion reasons are available to SQL clients.
-- A quota pool is (usage_provider, login, long_window), regardless of models or groups.
CREATE VIEW model_group_candidates WITH (security_invoker = true) AS
WITH pooled AS (
 SELECT m.*, 2 * (SELECT count(*) FROM workflow_model_suggestions s
                  WHERE s.account_id=m.account_id AND s.usage_provider=m.usage_provider
                    AND s.login=m.login AND s.long_window=m.long_window
                    AND s.created_at > now() - interval '60 minutes') AS penalty
 FROM model_group_members m
), inputs AS (
 SELECT m.*, g.token, g.name AS group_name, g.revision, h.name AS host,
        c.remaining_pct, c.last_observed, c.resets_at,
        c.remaining_pct - 100 * extract(epoch FROM least(c.resets_at, coalesce(m.eligible_until,c.resets_at)) - now()) / nullif(c.window_seconds,0)
          - m.penalty AS score,
        CASE
          WHEN NOT m.enabled THEN 'Disabled'
          WHEN m.eligible_until <= now() THEN 'Plan ended'
          WHEN NOT EXISTS (SELECT 1 FROM jsonb_array_elements(h.providers) p
                           WHERE p->>'name'=m.provider AND p->>'available'='true') THEN 'Provider unavailable'
          WHEN NOT EXISTS (SELECT 1 FROM provider_models p WHERE p.host=h.name AND p.provider=m.provider
                           AND p.id=m.model AND ((cardinality(p.thinking)=0 AND m.thinking='') OR m.thinking=ANY(p.thinking))) THEN 'Model or thinking unavailable'
          WHEN NOT EXISTS (SELECT 1 FROM usage_active a WHERE a.host=h.name AND a.provider=m.usage_provider
                           AND a.account=m.login AND a.until > now() - interval '15 minutes'
                           AND a.since=(SELECT max(a2.since) FROM usage_active a2 WHERE a2.host=a.host AND a2.provider=a.provider)) THEN 'Inactive login'
          WHEN c.remaining_pct IS NULL OR c.stale OR c.window_seconds IS NULL OR c.window_seconds<=0 OR c.resets_at IS NULL THEN 'Meter unknown or stale'
          WHEN c.remaining_pct <= 5 THEN 'Quota reserve'
          WHEN EXISTS (SELECT 1 FROM unnest(m.gate_windows) w
                       LEFT JOIN usage_current gate ON gate.provider=m.usage_provider AND gate.account=m.login AND gate.window_id=w
                       WHERE gate.remaining_pct IS NULL OR gate.stale OR gate.remaining_pct<=5) THEN 'Short-window quota unavailable'
          WHEN EXISTS (SELECT 1 FROM usage_current gate
                       WHERE gate.provider=m.usage_provider AND gate.account=m.login
                         AND regexp_replace(gate.window_id,'[^/]+$','')=regexp_replace(m.long_window,'[^/]+$','')
                         AND gate.window_id<>m.long_window
                         AND (gate.window_seconds<c.window_seconds OR gate.window_seconds IS NULL)
                         AND (gate.remaining_pct IS NULL OR gate.stale OR gate.remaining_pct<=5)) THEN 'Short-window quota unavailable'
          ELSE '' END AS reason
 FROM pooled m JOIN model_groups g ON g.id=m.group_id AND g.account_id=m.account_id AND g.deleted_at IS NULL
 JOIN host_keys k ON k.account_id=m.account_id JOIN hosts h ON h.name=k.host
 LEFT JOIN usage_current c ON c.provider=m.usage_provider AND c.account=m.login AND c.window_id=m.long_window
)
SELECT *, reason='' AND remaining_pct - penalty > 5 AS eligible
FROM inputs;
