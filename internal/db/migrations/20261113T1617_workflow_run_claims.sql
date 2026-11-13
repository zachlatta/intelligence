-- Parallel runs on one deliverable (internal/workflow/steer.go). A run claims what it will change
-- or publish (a public link, a file, a named work item) before it builds; a claim holds while the
-- run is live, and another live run's claim on the same thing, or on a folder holding it, is
-- refused. A run that finds its work belongs to a live run steers into it instead, recorded as its
-- steered answer resolution (workflow_run_answer_resolutions).
CREATE TABLE workflow_run_claims (
 run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 deliverable text NOT NULL CHECK (length(deliverable) BETWEEN 1 AND 500),
 claimed_at timestamptz NOT NULL,
 PRIMARY KEY (run_id, deliverable)
);
CREATE INDEX workflow_run_claims_account ON workflow_run_claims (account_id, deliverable);
ALTER TABLE workflow_run_claims ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_claims FOR SELECT USING (account_id=current_account_id());

-- workflow_run_answer_status (0050) with steer_live at the end: whether the run at the end of a
-- steered run's chain is live, so may still answer it. Once that run is over without an answer
-- sent from the handover on, the steered request is plainly unanswered.
CREATE OR REPLACE VIEW workflow_run_answer_status WITH (security_invoker=true) AS
 WITH RECURSIVE hops (run_id, target, since, path) AS (
  SELECT s.run_id, s.steered_to, s.at, ARRAY[s.run_id, s.steered_to]
  FROM workflow_run_answer_resolutions s WHERE s.kind = 'steered'
  UNION ALL
  SELECT h.run_id, s.steered_to, greatest(h.since, s.at), h.path || s.steered_to
  FROM hops h JOIN workflow_run_answer_resolutions s ON s.run_id = h.target AND s.kind = 'steered'
  WHERE s.steered_to <> ALL (h.path)
 ), answered AS (
  SELECT a.run_id, a.run_id AS via, a.sent_at FROM workflow_run_answers a WHERE a.checked
  UNION ALL
  SELECT h.run_id, h.target, a.sent_at
  FROM hops h JOIN workflow_run_answers a ON a.run_id = h.target AND a.checked AND a.sent_at >= h.since
 ), first AS (
  SELECT DISTINCT ON (run_id) run_id, via, sent_at FROM answered ORDER BY run_id, sent_at, via = run_id DESC
 ), last AS (
  SELECT DISTINCT ON (h.run_id) h.run_id, t.status NOT IN ('succeeded','failed','canceled') AS live
  FROM hops h JOIN workflow_runs t ON t.id = h.target ORDER BY h.run_id, array_length(h.path, 1) DESC
 )
 SELECT r.id, r.account_id, r.answer_expected, f.sent_at AS answered_at, f.via AS answered_via,
 s.kind AS resolution, s.steered_to, s.reason, s.at AS resolved_at, coalesce(l.live, false) AS steer_live
 FROM workflow_runs r LEFT JOIN first f ON f.run_id = r.id LEFT JOIN workflow_run_answer_resolutions s ON s.run_id = r.id
 LEFT JOIN last l ON l.run_id = r.id;
