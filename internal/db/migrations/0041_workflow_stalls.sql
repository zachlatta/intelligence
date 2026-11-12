-- Recovery keeps the run and its gates intact. Attempts are durable and account-readable.
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_runs_status_check;
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_status_check CHECK
 (status IN ('queued','finding_project','starting','running','waiting','stalled','succeeded','failed','canceled'));
ALTER TABLE workflow_runs ADD COLUMN idle_since timestamptz;
ALTER TABLE workflow_runs ADD COLUMN idle_observation text NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN report_received_at timestamptz;
CREATE TABLE workflow_run_continuations (
 run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 event_id bigint NOT NULL REFERENCES workflow_run_events ON DELETE CASCADE,
 attempt integer NOT NULL CHECK (attempt BETWEEN 1 AND 2),
 command_id uuid UNIQUE REFERENCES commands ON DELETE SET NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY(run_id,event_id,attempt)
);
ALTER TABLE workflow_run_continuations ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_continuations FOR SELECT USING (account_id=current_account_id());
-- Grants are applied by EnsureReadonlyRole along with every public relation.
ALTER TABLE workflow_runs ADD COLUMN stall_checked_at timestamptz;
CREATE INDEX workflow_runs_stall_check ON workflow_runs (stall_checked_at NULLS FIRST, id) WHERE status='running';

-- Labels and incoming context are not lifecycle transitions. Share this epoch between
-- attempt deduplication and command delivery so metadata cannot reset the recovery budget.
CREATE VIEW workflow_run_latest_transition WITH (security_invoker = true) AS
SELECT run_id, max(id) AS event_id FROM workflow_run_events
WHERE kind IN ('started','queued','dequeued','project','workspace','start_retry','start_retry_sent',
              'error','stalled','next','skip','back','ask','reply','resume','done','fail','cancel')
GROUP BY run_id;
