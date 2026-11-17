-- A percentile is the margin: do not multiply it by another 25%. Continuous
-- p85 interpolates sparse histories instead of always taking their maximum.
-- Prefer a fortnight of recent work; retain ninety-day evidence for quiet repos.
-- repo NULL identifies a workflow's history across repositories, for new projects.
CREATE OR REPLACE VIEW workflow_memory_estimate WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.workflow_id, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil(percentile_cont(0.85) WITHIN GROUP (ORDER BY m.peak_memory_bytes)))::bigint
        AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at >= CURRENT_TIMESTAMP - interval '14 days'
    AND r.finished_at <= CURRENT_TIMESTAMP
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY GROUPING SETS ((r.account_id, r.host, r.workflow_id, r.repo), (r.account_id, r.host, r.repo), (r.account_id, r.host, r.workflow_id));

CREATE VIEW workflow_memory_estimate_history WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.workflow_id, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil(percentile_cont(0.85) WITHIN GROUP (ORDER BY m.peak_memory_bytes)))::bigint
        AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at >= CURRENT_TIMESTAMP - interval '90 days'
    AND r.finished_at <= CURRENT_TIMESTAMP
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY GROUPING SETS ((r.account_id, r.host, r.workflow_id, r.repo), (r.account_id, r.host, r.repo), (r.account_id, r.host, r.workflow_id));

COMMENT ON VIEW workflow_memory_estimate IS 'Fourteen-day continuous p85; repo null means the workflow across repositories; workflow_id null means the repository across workflows';
COMMENT ON VIEW workflow_memory_estimate_history IS 'Ninety-day fallback for quiet workflows and repositories; same grouping and percentile as workflow_memory_estimate';

-- Refresh unfinished selected runs; completed runs keep their charged estimate.
UPDATE workflow_runs r SET estimated_memory_bytes = coalesce(
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id=r.workflow_id AND m.repo=r.repo),
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate_history m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id=r.workflow_id AND m.repo=r.repo),
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id=r.workflow_id AND m.repo IS NULL),
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate_history m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id=r.workflow_id AND m.repo IS NULL),
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id IS NULL AND m.repo=r.repo),
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate_history m WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id IS NULL AND m.repo=r.repo),4294967296)
WHERE r.status NOT IN ('succeeded','failed','canceled') AND r.project_selected;
