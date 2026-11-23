DROP INDEX IF EXISTS workflow_runs_retried_from_idx;

CREATE UNIQUE INDEX workflow_runs_retried_from_unique
 ON workflow_runs (retried_from_run_id)
 WHERE retried_from_run_id IS NOT NULL;
