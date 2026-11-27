-- Switch active workflows whose orchestrator is claude/claude-opus-5-5 to
-- antigravity/gemini-4-neon-high. Antigravity selects thinking/effort through the model ID,
-- so clear orchestrator.thinking. Save a new version so existing runs keep their history;
-- leave other orchestrator models, project pickers, instructions and deleted workflows alone.
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
        IF btrim(coalesce(w.definition #>> '{orchestrator,provider}', '')) = 'claude'
           AND btrim(coalesce(w.definition #>> '{orchestrator,model}', '')) = 'claude-opus-5-5' THEN
            updated := jsonb_set(w.definition, '{orchestrator,provider}', '"antigravity"'::jsonb);
            updated := jsonb_set(updated, '{orchestrator,model}', '"gemini-4-neon-high"'::jsonb);
            updated := updated #- '{orchestrator,thinking}';
            IF updated <> w.definition THEN
                INSERT INTO workflow_versions(workflow_id, version, account_id, definition, created_at)
                    VALUES(w.id, w.version + 1, w.account_id, updated, now());
                UPDATE workflows SET version = w.version + 1, updated_at = now() WHERE id = w.id;
            END IF;
        END IF;
    END LOOP;
END $$;
