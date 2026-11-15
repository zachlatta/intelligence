-- Estimate a run's memory from its own workflow's runs in its repository. One repository holds
-- very different work: in a desk repository a two-minute email answer peaks at a few hundred MiB,
-- while its Factory Task runs reach several GiB, and the repository-wide estimate charged every
-- email answer like a Factory Task.
--
-- One rule, for both: what nine successful-run peaks in ten stayed under (the heaviest, until there
-- are ten), plus 25%, at least 1 GiB. Rows with a workflow_id are per workflow and repository; rows
-- with a null workflow_id (a run always has one) are the repository's, over all its workflows, and
-- are what workflow_repository_memory reads. A run takes its workflow's estimate, else the
-- repository's, else 4 GiB (internal/workflow/memory.go).
CREATE VIEW workflow_memory_estimate WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.workflow_id, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil((percentile_disc(0.9) WITHIN GROUP (ORDER BY m.peak_memory_bytes)) * 1.25))::bigint
        AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at IS NOT NULL
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY GROUPING SETS ((r.account_id, r.host, r.workflow_id, r.repo), (r.account_id, r.host, r.repo));
COMMENT ON VIEW workflow_memory_estimate IS 'Memory estimate per workflow and repository (workflow_id null: the whole repository)';

CREATE OR REPLACE VIEW workflow_repository_memory WITH (security_invoker = true) AS
SELECT account_id, host, repo, observed_runs, estimated_memory_bytes
FROM workflow_memory_estimate WHERE workflow_id IS NULL;

-- Re-estimate unfinished runs that chose their project, so runs queued behind another workflow's
-- estimate move on deploy. Finished runs keep the estimate they ran with.
UPDATE workflow_runs r SET estimated_memory_bytes = m.estimated_memory_bytes
FROM workflow_memory_estimate m
WHERE r.status NOT IN ('succeeded', 'failed', 'canceled') AND r.project_selected
    AND m.account_id = r.account_id AND m.host = r.host AND m.workflow_id = r.workflow_id AND m.repo = r.repo;
