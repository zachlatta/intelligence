ALTER TABLE workflow_runs
    ADD COLUMN start_retries integer NOT NULL DEFAULT 0 CHECK (start_retries BETWEEN 0 AND 3),
    ADD COLUMN start_retry_at timestamptz;

-- Retry deliveries have distinct command IDs so the host's completed-result cache cannot
-- replay a failed attempt. All deliveries use the first command's Paseo creation key.
ALTER TABLE commands ADD COLUMN idempotency_key text NOT NULL DEFAULT '';
-- Older host binaries ignore the new wire key, so wait for an updated host report.
ALTER TABLE hosts ADD COLUMN start_retry_keys boolean NOT NULL DEFAULT false;
CREATE INDEX workflow_runs_pending_start ON workflow_runs (pending_command)
    WHERE pending_command IS NOT NULL AND status IN ('finding_project', 'starting');
