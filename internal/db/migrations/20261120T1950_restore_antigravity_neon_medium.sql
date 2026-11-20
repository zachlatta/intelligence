UPDATE provider_plan_settings SET enabled = true WHERE provider = 'antigravity';

-- Group editors read the revision before writing. Wait for edits already in
-- progress and make later editors read the new revision.
LOCK TABLE model_groups IN ACCESS EXCLUSIVE MODE;

WITH replacements AS (
    SELECT m.id, EXISTS (
        SELECT 1 FROM model_group_members medium
        WHERE medium.group_id = m.group_id AND medium.provider = m.provider
          AND medium.model = 'gemini-4-neon-medium' AND medium.thinking = ''
          AND medium.login = m.login AND medium.long_window = m.long_window
    ) AS already_present
    FROM model_group_members m
    JOIN model_groups g ON g.id = m.group_id
    WHERE g.token = 'frontier_models_model' AND g.deleted_at IS NULL
      AND m.provider = 'antigravity' AND m.model = 'gemini-4-neon-xhigh'
), removed AS (
    DELETE FROM model_group_members m USING replacements r
    WHERE m.id = r.id AND r.already_present
    RETURNING m.group_id
), replaced AS (
    UPDATE model_group_members m SET model = 'gemini-4-neon-medium', thinking = ''
    FROM replacements r WHERE m.id = r.id AND NOT r.already_present
    RETURNING m.group_id
)
UPDATE model_groups SET revision = revision + 1
WHERE id IN (SELECT group_id FROM removed UNION ALL SELECT group_id FROM replaced);
