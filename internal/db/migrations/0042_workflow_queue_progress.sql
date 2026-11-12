-- Keep each host's run queue moving.
--
-- started_by_hand: Start now was pressed for the run. It keeps that bypass through project
-- selection and start retries, so a run started ahead of the queue never waits behind it again.
ALTER TABLE workflow_runs ADD COLUMN started_by_hand boolean NOT NULL DEFAULT false;
UPDATE workflow_runs r SET started_by_hand = true
WHERE EXISTS (SELECT 1 FROM workflow_run_events e
    WHERE e.run_id = r.id AND e.kind = 'dequeued' AND e.text = 'started by hand, ahead of the queue');

-- A repository's estimate is what nine of its runs in ten stayed under (the heaviest, until it
-- has ten), plus 25%, at least 1 GiB. The heaviest run alone made every run reserve its peak.
CREATE OR REPLACE VIEW workflow_repository_memory WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil((percentile_disc(0.9) WITHIN GROUP (ORDER BY m.peak_memory_bytes)) * 1.25))::bigint
        AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at IS NOT NULL
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY r.account_id, r.host, r.repo;

-- Re-estimate unfinished runs, so the queue already held by the old estimates moves on deploy.
-- Finished runs keep the estimate they ran with.
UPDATE workflow_runs r SET estimated_memory_bytes = m.estimated_memory_bytes
FROM workflow_repository_memory m
WHERE r.status NOT IN ('succeeded', 'failed', 'canceled') AND r.project_selected
    AND m.account_id = r.account_id AND m.host = r.host AND m.repo = r.repo;
