-- Step models lived inside JSON, not separate columns. Remove those keys everywhere a
-- definition can be loaded (including old versions used by runs and proposal undo snapshots).
-- Preserve choices as instructions so model groups keep using the original token mechanism.
-- Transform snapshots identically to avoid introducing false proposal drift.
CREATE FUNCTION public.migrate_step_models(def jsonb) RETURNS jsonb
LANGUAGE plpgsql STRICT AS $$
DECLARE
    steps jsonb := '[]';
    step jsonb;
    model text;
    instructions text;
BEGIN
    IF jsonb_typeof(def->'steps') IS DISTINCT FROM 'array' THEN
        RETURN def;
    END IF;
    FOR step IN SELECT value FROM jsonb_array_elements(def->'steps') LOOP
        model := coalesce(step->>'model', '');
        instructions := coalesce(step->>'instructions', '');
        IF model ~ '^\{\{[a-z][a-z0-9_]*_model\}\}$' THEN
            IF position(model IN instructions) = 0 THEN
                instructions := instructions || E'\n\nUse ' || model || ' for this step''s workers.';
            END IF;
        ELSIF model <> '' THEN
            instructions := instructions || E'\n\nUse model ' || model ||
                CASE WHEN coalesce(step->>'thinking', '') <> ''
                    THEN ' with thinking ' || (step->>'thinking') ELSE '' END ||
                ' for this step''s workers.';
        END IF;
        IF model <> '' THEN
            step := jsonb_set(step, '{instructions}', to_jsonb(instructions));
        END IF;
        steps := steps || jsonb_build_array(step - 'model' - 'thinking');
    END LOOP;
    RETURN jsonb_set(def, '{steps}', steps);
END $$;

UPDATE workflow_versions SET definition = public.migrate_step_models(definition)
WHERE definition IS DISTINCT FROM public.migrate_step_models(definition);

UPDATE workflow_proposals SET
    before_state = CASE WHEN before_state ? 'definition' THEN jsonb_set(before_state, '{definition}', public.migrate_step_models(before_state->'definition')) ELSE before_state END,
    proposed_state = CASE WHEN proposed_state ? 'definition' THEN jsonb_set(proposed_state, '{definition}', public.migrate_step_models(proposed_state->'definition')) ELSE proposed_state END,
    after_state = CASE WHEN after_state ? 'definition' THEN jsonb_set(after_state, '{definition}', public.migrate_step_models(after_state->'definition')) ELSE after_state END,
    undo_state = CASE WHEN undo_state ? 'definition' THEN jsonb_set(undo_state, '{definition}', public.migrate_step_models(undo_state->'definition')) ELSE undo_state END;

DROP FUNCTION public.migrate_step_models(jsonb);
