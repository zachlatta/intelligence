-- A ready retry joins the same FIFO as a new run while retaining its startup
-- phase. Backoff and recovery waits do not reserve a place in projected load.
ALTER TABLE workflow_runs ADD COLUMN start_retry_queued boolean NOT NULL DEFAULT false
    CHECK (NOT start_retry_queued OR (start_retry_at IS NOT NULL AND status IN ('finding_project', 'starting')));
CREATE INDEX workflow_runs_retry_queued ON workflow_runs (host, created_at) WHERE start_retry_queued;

CREATE OR REPLACE VIEW workflow_run_queue WITH (security_invoker = true) AS
SELECT id AS run_id, account_id, host, workflow_id, title,
       row_number() OVER (PARTITION BY host ORDER BY created_at, id) AS position,
       queue_reason, created_at AS queued_at
FROM workflow_runs
WHERE status = 'queued' OR start_retry_queued;
