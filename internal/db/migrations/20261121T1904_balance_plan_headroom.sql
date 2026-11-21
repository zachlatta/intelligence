-- Spend fixed-reset quota before it expires. A reset a full window away
-- scores like a fresh window, whether or not it has started counting usage.
-- A forward shift between in-progress readings makes the deadline uncertain.
CREATE OR REPLACE VIEW model_group_candidates WITH (security_invoker = true) AS
WITH pooled AS (
 SELECT m.*, 2 * (SELECT count(*) FROM workflow_model_suggestions s
                  WHERE s.account_id=m.account_id AND s.provider=m.provider
                    AND s.login=m.login
                    -- Fable also spends the shared Claude weekly allowance.
                    AND (s.long_window=m.long_window OR
                         (m.provider='claude' AND m.long_window IN ('7d','fable/7d')
                          AND s.long_window IN ('7d','fable/7d')))
                    AND s.created_at > now() - interval '60 minutes') AS penalty
 FROM model_group_members m
), inputs AS (
 SELECT m.*, g.token, g.name AS group_name, g.revision, h.name AS host,
        c.remaining_pct, c.last_observed, c.resets_at,
        headroom.remaining_pct - CASE
          WHEN (m.eligible_until IS NULL OR m.eligible_until >= c.resets_at)
            AND c.used_pct > 0 AND prior.used_pct > 0
            AND extract(epoch FROM c.resets_at - c.last_observed) < c.window_seconds - 60
            AND extract(epoch FROM prior.resets_at - prior.last_observed) < c.window_seconds - 60
            AND prior.last_observed >= c.last_observed - interval '15 minutes'
            AND prior.resets_at > c.last_observed
            AND c.resets_at > prior.resets_at + interval '1 minute'
          THEN 0
          ELSE 100 * extract(epoch FROM least(c.resets_at, m.eligible_until) - now()) / nullif(c.window_seconds,0)
        END
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
          WHEN limits.fable AND (shared.remaining_pct IS NULL OR shared.stale OR shared.resets_at IS NULL
                               OR scoped.remaining_pct IS NULL OR scoped.stale OR scoped.resets_at IS NULL) THEN 'Fable or shared weekly meter unknown or stale'
          WHEN headroom.remaining_pct <= 5 THEN 'Quota reserve'
          WHEN EXISTS (SELECT 1 FROM usage_current gate
                       WHERE gate.provider=m.provider AND gate.account=m.login
                         AND gate.window_id<>m.long_window
                         AND ((regexp_replace(gate.window_id,'[^/]+$','')=regexp_replace(m.long_window,'[^/]+$','')
                               AND (gate.window_seconds<c.window_seconds OR gate.window_seconds IS NULL))
                              OR (limits.fable AND gate.window_id='5h'))
                         AND (gate.remaining_pct IS NULL OR gate.stale OR gate.resets_at IS NULL OR gate.remaining_pct<=5)) THEN 'Short-window quota unavailable'
          WHEN headroom.remaining_pct - m.penalty <= 5 THEN 'Quota reserved for recent suggestions'
          ELSE '' END AS reason
 FROM pooled m JOIN model_groups g ON g.id=m.group_id AND g.account_id=m.account_id AND g.deleted_at IS NULL
 JOIN host_keys k ON k.account_id=m.account_id JOIN hosts h ON h.name=k.host
 LEFT JOIN usage_current c ON c.provider=m.provider AND c.account=m.login AND c.window_id=m.long_window
 LEFT JOIN LATERAL (
   SELECT r.resets_at, r.last_observed, r.used_pct
   FROM usage_readings r
   WHERE r.provider=c.provider AND r.account=c.account AND r.window_id=c.window_id
     AND r.last_observed < c.last_observed
   ORDER BY r.first_observed DESC LIMIT 1
 ) prior ON true
 CROSS JOIN LATERAL (SELECT m.provider='claude' AND m.model LIKE 'claude-fable-%' AS fable) limits
 LEFT JOIN usage_current shared ON limits.fable AND shared.provider=m.provider AND shared.account=m.login AND shared.window_id='7d'
 LEFT JOIN usage_current scoped ON limits.fable AND scoped.provider=m.provider AND scoped.account=m.login AND scoped.window_id='fable/7d'
 CROSS JOIN LATERAL (SELECT least(c.remaining_pct, shared.remaining_pct, scoped.remaining_pct) AS remaining_pct) headroom
)
SELECT *, reason='' AS eligible
FROM inputs;
