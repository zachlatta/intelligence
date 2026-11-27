-- Model-only variants: a workflow that follows another one (its source) keeps only its own
-- orchestrator agent (provider, model, thinking, serviceTier) and its own trigger (trigger,
-- apiTrigger, email, voiceMemo). Everything else in its definition is its source's current one,
-- saved again as a new version of the variant whenever the source is saved (internal/workflow,
-- variant.go). The column is NULL for a workflow that follows none.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS variant_of uuid REFERENCES workflows(id);
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_variant_of_other;
ALTER TABLE workflows ADD CONSTRAINT workflows_variant_of_other CHECK (variant_of IS NULL OR variant_of <> id);
CREATE INDEX IF NOT EXISTS workflows_variant_of ON workflows (variant_of) WHERE variant_of IS NOT NULL;

-- Zach Email Task (GPT-6.2 Sol Ultrafast) was a copy of Zach Email Task v13 made to run the
-- orchestrator on GPT-6.2 Sol Ultrafast, and missed v14. Make it a variant of Zach Email Task, as
-- a new version of it: the source's current definition with the copy's own orchestrator agent and
-- trigger (its own email address). Its id, name, earlier versions and runs stay. Matched by id and
-- name in one account; a no-op anywhere else, and once it is a variant.
DO $$
DECLARE
    src record;
    cp record;
    updated jsonb;
    k text;
BEGIN
    SELECT w.id, w.account_id, v.definition INTO src
    FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
    WHERE w.id = '08b5cc05-9fb2-4a96-8bef-1d9c5de89701' AND w.name = 'Zach Email Task'
      AND w.deleted_at IS NULL AND w.variant_of IS NULL;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT w.id, w.version, v.definition INTO cp
    FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
    WHERE w.id = '151dc53e-6a0c-4cb5-8e71-b156461a1c03' AND w.name = 'Zach Email Task (GPT-6.2 Sol Ultrafast)'
      AND w.account_id = src.account_id AND w.deleted_at IS NULL AND w.variant_of IS NULL;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM workflows WHERE variant_of = cp.id) THEN
        RETURN;
    END IF;

    updated := src.definition;
    FOREACH k IN ARRAY ARRAY['trigger', 'apiTrigger', 'email', 'voiceMemo'] LOOP
        IF cp.definition ? k THEN
            updated := jsonb_set(updated, ARRAY[k], cp.definition -> k);
        ELSE
            updated := updated - k;
        END IF;
    END LOOP;
    FOREACH k IN ARRAY ARRAY['provider', 'model', 'thinking', 'serviceTier'] LOOP
        IF (cp.definition -> 'orchestrator') ? k THEN
            updated := jsonb_set(updated, ARRAY['orchestrator', k], cp.definition -> 'orchestrator' -> k);
        ELSE
            updated := updated #- ARRAY['orchestrator', k];
        END IF;
    END LOOP;

    UPDATE workflows SET variant_of = src.id WHERE id = cp.id;
    IF updated <> cp.definition THEN
        INSERT INTO workflow_versions(workflow_id, version, account_id, definition, created_at)
            VALUES (cp.id, cp.version + 1, src.account_id, updated, now());
        UPDATE workflows SET version = cp.version + 1, updated_at = now() WHERE id = cp.id;
    END IF;
END $$;
