-- How a run that owes an answer settled it without sending one itself: it handed its request to
-- another run (steered, answered when that run answers), no answer was due (not_needed, with the
-- reason), or it was requested before answer recording started (legacy, marked once below).
CREATE TABLE workflow_run_answer_resolutions (
 run_id uuid PRIMARY KEY REFERENCES workflow_runs ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 kind text NOT NULL CHECK (kind IN ('steered','not_needed','legacy')),
 steered_to uuid REFERENCES workflow_runs ON DELETE CASCADE,
 reason text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
 at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((kind = 'steered') = (steered_to IS NOT NULL)),
 CHECK (steered_to <> run_id),
 CHECK (kind = 'steered' OR btrim(reason) <> '')
);
CREATE INDEX workflow_run_answer_resolutions_steered_to ON workflow_run_answer_resolutions (steered_to) WHERE steered_to IS NOT NULL;
ALTER TABLE workflow_run_answer_resolutions ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_answer_resolutions FOR SELECT USING (account_id=current_account_id());

-- Runs created before migration 0047 (answer recording) were never told to record an answer, so
-- the ones without a checked answer can never get one. Mark them once, from the time 0047 ran on
-- this database; a new database has no such runs.
INSERT INTO workflow_run_answer_resolutions (run_id, account_id, kind, reason, at)
SELECT r.id, r.account_id, 'legacy', 'Requested before answer recording started', m.applied_at
FROM workflow_runs r JOIN schema_migrations m ON m.version = '0047_workflow_answers'
WHERE r.answer_expected AND r.created_at < m.applied_at
 AND NOT EXISTS (SELECT 1 FROM workflow_run_answers a WHERE a.run_id = r.id AND a.checked);

-- Each run's answer: its first checked answer, its own or, through a chain of steers, the run
-- that took its request (counted only from when the request reached that run).
CREATE VIEW workflow_run_answer_status WITH (security_invoker=true) AS
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
 )
 SELECT r.id, r.account_id, r.answer_expected, f.sent_at AS answered_at, f.via AS answered_via,
 s.kind AS resolution, s.steered_to, s.reason, s.at AS resolved_at
 FROM workflow_runs r LEFT JOIN first f ON f.run_id = r.id LEFT JOIN workflow_run_answer_resolutions s ON s.run_id = r.id;

-- Unchanged columns first (CREATE OR REPLACE keeps them); steered_to says which run a still
-- unanswered steered request is waiting on.
CREATE OR REPLACE VIEW workflow_runs_unanswered WITH (security_invoker=true) AS
 SELECT r.id, r.account_id, r.workflow_id, r.host, r.title, r.status, r.requested_at, r.answer_expected,
 extract(epoch FROM (now()-r.requested_at))/60 AS waiting_minutes,
 EXISTS (SELECT 1 FROM workflow_run_answers a WHERE a.run_id=r.id AND NOT a.checked) AS has_unchecked_sends,
 st.steered_to
 FROM workflow_runs r JOIN workflow_run_answer_status st ON st.id = r.id
 WHERE r.answer_expected AND r.status <> 'canceled'
 AND r.requested_at < now()-interval '30 minutes'
 AND st.answered_at IS NULL
 AND st.resolution IS DISTINCT FROM 'not_needed' AND st.resolution IS DISTINCT FROM 'legacy';
