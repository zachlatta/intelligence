-- Version saved definitions rather than changing the snapshots used by existing runs.
-- Email (including voice email) and voice memo requests should start without a capacity hold.
DO $$
DECLARE w record;
BEGIN
    FOR w IN
        SELECT f.id, f.account_id, f.version, v.definition
        FROM workflows f JOIN workflow_versions v ON v.workflow_id=f.id AND v.version=f.version
        WHERE f.deleted_at IS NULL AND coalesce(v.definition->>'priority', '') = ''
    LOOP
        INSERT INTO workflow_versions(workflow_id, version, account_id, definition, created_at)
        VALUES(w.id, w.version+1, w.account_id, w.definition || jsonb_build_object('priority',
            CASE WHEN w.definition->>'trigger' IN ('email','voice_memo') THEN 'immediate' ELSE 'normal' END), now());
        UPDATE workflows SET version=w.version+1, updated_at=now() WHERE id=w.id;
    END LOOP;
END $$;

CREATE OR REPLACE VIEW workflow_run_queue WITH (security_invoker = true) AS
SELECT r.id AS run_id, r.account_id, r.host, r.workflow_id,
       coalesce(nullif(r.task_summary, ''), r.title) AS title,
       row_number() OVER (PARTITION BY r.host ORDER BY
           (coalesce(v.definition->>'priority','normal') = 'immediate') DESC, r.created_at, r.id) AS position,
       r.queue_reason, r.created_at AS queued_at,
       coalesce(v.definition->>'priority','normal') AS priority
FROM workflow_runs r JOIN workflow_versions v USING (workflow_id,version)
WHERE r.status = 'queued' OR r.start_retry_queued;
