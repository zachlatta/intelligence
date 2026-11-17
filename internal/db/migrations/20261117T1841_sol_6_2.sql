-- Upgrade saved choices, keeping the quota pool and thinking level. Bump each edited
-- group's revision once so an editor opened before the upgrade cannot overwrite it.
-- Deleted groups and historical suggestions remain unchanged.
WITH changed AS (
    UPDATE model_group_members m SET model = 'gpt-6.2-sol'
    FROM model_groups g
    WHERE g.id = m.group_id AND g.deleted_at IS NULL
      AND m.provider = 'codex' AND m.model = 'gpt-6.1-sol'
    RETURNING m.group_id
)
UPDATE model_groups SET revision = revision + 1
WHERE id IN (SELECT group_id FROM changed);

-- New runs use a new version; existing runs keep their original definition.
-- Only change launch models and instruction fields, never repository paths or metadata.
DO $$
DECLARE
    w record;
    updated jsonb;
    field_path text[];
    old_text text;
    new_text text;
BEGIN
    FOR w IN
        SELECT f.id, f.account_id, f.version, v.definition
        FROM workflows f JOIN workflow_versions v ON v.workflow_id = f.id AND v.version = f.version
        WHERE f.deleted_at IS NULL
    LOOP
        updated := w.definition;
        FOR field_path IN
            SELECT ARRAY['orchestrator', 'instructions']
            UNION ALL SELECT ARRAY['project', 'instructions']
            UNION ALL SELECT ARRAY['orchestrator', 'model']
                WHERE w.definition #>> '{orchestrator,provider}' = 'codex'
                  AND w.definition #>> '{orchestrator,model}' = 'gpt-6.1-sol'
            UNION ALL SELECT ARRAY['project', 'model']
                WHERE w.definition #>> '{project,provider}' = 'codex'
                  AND w.definition #>> '{project,model}' = 'gpt-6.1-sol'
            UNION ALL SELECT ARRAY['steps', (n - 1)::text, 'instructions']
                FROM jsonb_array_elements(coalesce(w.definition -> 'steps', '[]')) WITH ORDINALITY AS s(step, n)
        LOOP
            old_text := updated #>> field_path;
            IF old_text IS NULL THEN
                CONTINUE;
            END IF;
            new_text := replace(old_text, 'gpt-6.1-sol', 'gpt-6.2-sol');
            new_text := regexp_replace(new_text, '(GPT[- ‑])6[.]1([- ‑]Sol)', '\16.2\2', 'g');
            IF new_text <> old_text THEN
                updated := jsonb_set(updated, field_path, to_jsonb(new_text));
            END IF;
        END LOOP;
        IF updated <> w.definition THEN
            INSERT INTO workflow_versions(workflow_id, version, account_id, definition, created_at)
                VALUES(w.id, w.version + 1, w.account_id, updated, now());
            UPDATE workflows SET version = w.version + 1, updated_at = now() WHERE id = w.id;
        END IF;
    END LOOP;
END $$;
