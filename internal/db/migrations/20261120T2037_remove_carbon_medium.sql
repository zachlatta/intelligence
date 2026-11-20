-- Let in-flight group edits finish before removing the member. Later edits
-- read the bumped revision, so a stale draft cannot silently restore it.
LOCK TABLE model_groups IN ACCESS EXCLUSIVE MODE;

WITH removed AS (
    DELETE FROM model_group_members m USING model_groups g
    WHERE m.group_id = g.id AND g.deleted_at IS NULL
      AND g.token = 'frontier_models_model'
      AND m.provider = 'antigravity' AND m.model = 'gemini-4-carbon-medium'
    RETURNING m.group_id
)
UPDATE model_groups SET revision = revision + 1
WHERE id IN (SELECT group_id FROM removed);
