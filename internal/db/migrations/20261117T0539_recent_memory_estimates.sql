-- Keep the existing workflow/repository split and p90 + 25% margin, but expire
-- old workloads after seven days. With fewer than ten samples discrete p90 is
-- the maximum; with no samples admission falls back to the repository, then 4 GiB.
CREATE OR REPLACE VIEW workflow_memory_estimate WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.workflow_id, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil((percentile_disc(0.9) WITHIN GROUP (ORDER BY m.peak_memory_bytes)) * 1.25))::bigint
        AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at >= CURRENT_TIMESTAMP - interval '7 days'
    AND r.finished_at <= CURRENT_TIMESTAMP
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY GROUPING SETS ((r.account_id, r.host, r.workflow_id, r.repo), (r.account_id, r.host, r.repo));

-- Give already-admitted and queued runs the new estimate on upgrade. Finished
-- runs retain their charged estimate. Unknown/empty projects retain their charge.
UPDATE workflow_runs r SET estimated_memory_bytes = coalesce(
    (SELECT estimated_memory_bytes FROM workflow_memory_estimate m
     WHERE m.account_id=r.account_id AND m.host=r.host AND m.workflow_id=r.workflow_id AND m.repo=r.repo),
    (SELECT estimated_memory_bytes FROM workflow_repository_memory m
     WHERE m.account_id=r.account_id AND m.host=r.host AND m.repo=r.repo), 4294967296)
WHERE r.status NOT IN ('succeeded','failed','canceled') AND r.project_selected AND r.repo <> '';
