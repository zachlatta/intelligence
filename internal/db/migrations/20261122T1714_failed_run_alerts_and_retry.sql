-- Failed answer-expected run phone alerts and run retry linkage.
--
-- 1. workflow_runs.retried_from_run_id links a retry run (manual Retry button or automatic
--    re-run when a workflow gets a new version) back to the failed run it replaced.
-- 2. workflow_run_failure_alerts records one phone notification per failed answer-expected run
--    (email, voice memo) through PDW's notify tool, so failures are reported at once and never
--    repeated across retries or server restarts.

ALTER TABLE workflow_runs
 ADD COLUMN retried_from_run_id uuid REFERENCES workflow_runs(id) ON DELETE SET NULL;

CREATE INDEX workflow_runs_retried_from_idx
 ON workflow_runs (retried_from_run_id)
 WHERE retried_from_run_id IS NOT NULL;

CREATE TABLE workflow_run_failure_alerts (
 run_id uuid PRIMARY KEY REFERENCES workflow_runs(id) ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 workflow_id uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
 title text NOT NULL DEFAULT '',
 error text NOT NULL DEFAULT '',
 opened_at timestamptz NOT NULL DEFAULT now(),
 notify boolean NOT NULL DEFAULT false,
 alert_text text NOT NULL DEFAULT '',
 alert_sent_at timestamptz,
 alert_attempts integer NOT NULL DEFAULT 0,
 alert_error text NOT NULL DEFAULT ''
);

CREATE INDEX workflow_run_failure_alerts_pending
 ON workflow_run_failure_alerts (opened_at)
 WHERE notify AND alert_sent_at IS NULL;

ALTER TABLE workflow_run_failure_alerts ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_failure_alerts FOR SELECT
 USING (account_id = current_account_id());
