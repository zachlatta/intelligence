-- Deployment-wide plan availability, independent of installed provider support and meter reports.
-- Unlisted providers are enabled. Re-enabling a subscription is an explicit data migration.
CREATE TABLE provider_plan_settings (
    provider text PRIMARY KEY,
    enabled boolean NOT NULL DEFAULT true
);
ALTER TABLE provider_plan_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY every_account ON provider_plan_settings FOR SELECT USING (true);
INSERT INTO provider_plan_settings(provider, enabled) VALUES ('muse', false), ('antigravity', false), ('gemini', false);

CREATE OR REPLACE VIEW usage_totals WITH (security_invoker = true) AS
SELECT a.provider,
       count(DISTINCT a.account)::int AS logins,
       count(DISTINCT c.account) FILTER (WHERE c.remaining_pct IS NOT NULL AND NOT c.stale)::int AS logins_read,
       coalesce(sum(c.remaining_pct) FILTER (WHERE NOT c.stale), 0) / 100 AS weeks_left
FROM usage_accounts a
LEFT JOIN usage_current c ON c.provider = a.provider AND c.account = a.account AND c.window_id = '7d'
WHERE NOT EXISTS (SELECT 1 FROM provider_plan_settings ps WHERE ps.provider=a.provider AND NOT ps.enabled)
GROUP BY a.provider;

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

-- Retire startup commands queued before plan policy existed. Do not interrupt running work.
UPDATE commands SET status = 'failed', finished_at = now(), error = 'subscription provider is disabled'
WHERE status IN ('queued', 'sent')
  AND kind IN ('create_agent', 'create_workspace', 'create_schedule', 'update_schedule')
  AND coalesce(args->>'provider', args#>>'{agent,provider}') IN (SELECT provider FROM provider_plan_settings WHERE NOT enabled);

-- Paseo owns schedule execution: ask its host to pause existing schedules, rather than
-- only changing the database mirror. Hosts also pause disabled providers on later reports.
INSERT INTO commands(host,kind,args,created_at,expires_at)
SELECT host, 'pause_schedule', jsonb_build_object('scheduleId',id), now(), now()+interval '24 hours'
FROM schedules WHERE provider IN (SELECT provider FROM provider_plan_settings WHERE NOT enabled) AND status='active';
