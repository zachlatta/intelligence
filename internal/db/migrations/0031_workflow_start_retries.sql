-- A failed or expired start is retried (internal/workflow/retry.go).
--   start_retries:      retries sent in the current startup phase
--   start_retry_at:     when the next retry is due; null when none is scheduled
--   start_retry_queued: a due retry whose host is ready waits in the same FIFO as a new run,
--                       keeping its startup phase. Backoff and recovery waits do not reserve a
--                       place in projected load.
ALTER TABLE workflow_runs
    ADD COLUMN start_retries integer NOT NULL DEFAULT 0 CHECK (start_retries BETWEEN 0 AND 3),
    ADD COLUMN start_retry_at timestamptz,
    ADD COLUMN start_retry_queued boolean NOT NULL DEFAULT false
        CHECK (NOT start_retry_queued OR (start_retry_at IS NOT NULL AND status IN ('finding_project', 'starting')));
CREATE INDEX workflow_runs_pending_start ON workflow_runs (pending_command)
    WHERE pending_command IS NOT NULL AND status IN ('finding_project', 'starting');
CREATE INDEX workflow_runs_retry_queued ON workflow_runs (host, created_at) WHERE start_retry_queued;

-- Retry deliveries have distinct command IDs so the host's completed-result cache cannot
-- replay a failed attempt. All deliveries use the first command's Paseo creation key.
ALTER TABLE commands ADD COLUMN idempotency_key text NOT NULL DEFAULT '';
-- Older host binaries ignore the new wire key, so wait for an updated host report.
ALTER TABLE hosts ADD COLUMN start_retry_keys boolean NOT NULL DEFAULT false;

-- Each host's queue in the order its runs start: new runs and ready retries. Position 1 is next.
CREATE OR REPLACE VIEW workflow_run_queue WITH (security_invoker = true) AS
SELECT id AS run_id, account_id, host, workflow_id, title,
       row_number() OVER (PARTITION BY host ORDER BY created_at, id) AS position,
       queue_reason, created_at AS queued_at
FROM workflow_runs
WHERE status = 'queued' OR start_retry_queued;
