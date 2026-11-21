-- Add Neon Extra High to the existing frontier token for accounts with an
-- observed Antigravity Gemini pool. The candidate view still enforces provider
-- policy, host catalog availability, an active login and fresh quota.
-- Group edits write the group before replacing its members. Serialize with edits
-- from an older server so the append and revision bump use the same saved group.
LOCK TABLE model_groups IN SHARE ROW EXCLUSIVE MODE;

WITH inserted AS (
    INSERT INTO model_group_members (account_id, group_id, position, provider, model, thinking, login, long_window)
    SELECT g.account_id, g.id,
           coalesce((SELECT max(m.position) FROM model_group_members m WHERE m.group_id = g.id), -1) + 1,
           'antigravity', 'gemini-4-neon-xhigh', '', 'default', 'gemini/7d'
    FROM model_groups g
    WHERE g.token = 'frontier_models_model' AND g.deleted_at IS NULL
      AND EXISTS (
          SELECT 1 FROM host_keys k JOIN usage_sources s ON s.host = k.host
          WHERE k.account_id = g.account_id AND s.provider = 'antigravity' AND s.account = 'default'
      )
      AND EXISTS (
          SELECT 1 FROM usage_current c
          WHERE c.provider = 'antigravity' AND c.account = 'default'
            AND c.window_id = 'gemini/7d' AND c.window_seconds = 604800
      )
      AND NOT EXISTS (
          SELECT 1 FROM model_group_members m
          WHERE m.group_id = g.id AND m.provider = 'antigravity'
            AND m.model = 'gemini-4-neon-xhigh' AND m.login = 'default'
      )
      AND (SELECT count(*) FROM model_group_members m WHERE m.group_id = g.id) < 100
    RETURNING group_id
)
UPDATE model_groups SET revision = revision + 1
WHERE id IN (SELECT group_id FROM inserted);
