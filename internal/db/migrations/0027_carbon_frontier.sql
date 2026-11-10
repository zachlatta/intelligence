-- Prefer Carbon for new runs. Keep old versions intact: running and historical runs reference
-- them, and Argon remains a supported provider model. Only touch model choices and prompts,
-- not workflow metadata, repository paths or task inputs. Custom non-Gemini choices stay put.
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
                WHERE w.definition #>> '{orchestrator,provider}' = 'antigravity'
            UNION ALL SELECT ARRAY['project', 'model']
                WHERE w.definition #>> '{project,provider}' = 'antigravity'
            UNION ALL SELECT ARRAY['steps', (n - 1)::text, 'instructions']
                FROM jsonb_array_elements(coalesce(w.definition -> 'steps', '[]')) WITH ORDINALITY AS s(step, n)
        LOOP
            old_text := updated #>> field_path;
            IF old_text IS NULL THEN
                CONTINUE;
            END IF;
            new_text := replace(replace(replace(replace(replace(old_text,
                'gemini-4-argon-low', 'gemini-4-carbon-low'),
                'gemini-4-argon-medium', 'gemini-4-carbon-medium'),
                'gemini-4-argon-high', 'gemini-4-carbon-high'),
                'Gemini 4 Argon', 'Gemini 4 Carbon'),
                'Gemini Argon 4', 'Gemini 4 Carbon');
            new_text := replace(new_text,
                'Use frontier models, and spread the work across the plans: claude/claude-opus-5-5 and codex.',
                'Use frontier models, and spread the work across the plans: claude/claude-opus-5-5, codex and antigravity/gemini-4-carbon-high (Gemini 4 Carbon).');
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
