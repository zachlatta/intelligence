-- Antigravity selects thinking/effort through the model ID (e.g. gemini-4-neon-medium)
-- and Paseo rejects any non-empty thinkingOptionId for antigravity agents.
-- Remove leftover thinking keys from active workflows whose orchestrator or project
-- finder uses antigravity, saving a new version so existing runs keep their history.
DO $$
DECLARE
    w record;
    updated jsonb;
BEGIN
    FOR w IN
        SELECT f.id, f.account_id, f.version, v.definition
        FROM workflows f
        JOIN workflow_versions v ON v.workflow_id = f.id AND v.version = f.version
        WHERE f.deleted_at IS NULL
    LOOP
        updated := w.definition;
        IF btrim(coalesce(updated #>> '{orchestrator,provider}', '')) = 'antigravity'
           AND btrim(coalesce(updated #>> '{orchestrator,thinking}', '')) <> '' THEN
            updated := updated #- '{orchestrator,thinking}';
        END IF;
        IF btrim(coalesce(updated #>> '{project,provider}', '')) = 'antigravity'
           AND btrim(coalesce(updated #>> '{project,thinking}', '')) <> '' THEN
            updated := updated #- '{project,thinking}';
        END IF;
        IF updated <> w.definition THEN
            INSERT INTO workflow_versions(workflow_id, version, account_id, definition, created_at)
                VALUES(w.id, w.version + 1, w.account_id, updated, now());
            UPDATE workflows SET version = w.version + 1, updated_at = now() WHERE id = w.id;
        END IF;
    END LOOP;
END $$;
