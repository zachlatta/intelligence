-- A short aborted run is not evidence that a repository's full workload is small.
CREATE OR REPLACE VIEW workflow_repository_memory WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil(max(m.peak_memory_bytes) * 1.25))::bigint AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.status = 'succeeded' AND r.finished_at IS NOT NULL
    AND r.project_selected AND r.repo <> '' AND m.peak_memory_bytes > 0
GROUP BY r.account_id, r.host, r.repo;

-- Raise any existing poisoned reservations; never lower an admitted snapshot.
UPDATE workflow_runs r SET estimated_memory_bytes = greatest(r.estimated_memory_bytes,
    coalesce((SELECT m.estimated_memory_bytes FROM workflow_repository_memory m
        WHERE m.account_id=r.account_id AND m.host=r.host AND m.repo=r.repo),4294967296))
WHERE r.finished_at IS NULL;

-- Preserve submission vs launch semantics even when timestamps tie or a run requeues.
ALTER TABLE workflow_runs ADD COLUMN queued_at_creation boolean NOT NULL DEFAULT false;
UPDATE workflow_runs r SET queued_at_creation = true
WHERE r.launched_at IS NULL
    AND r.project_command IS NULL AND r.launch_command IS NULL AND r.pending_command IS NULL
    AND r.project_agent_id='' AND r.orchestrator_id='' AND r.workspace_id=''
    AND EXISTS (SELECT 1 FROM workflow_run_events e WHERE e.run_id=r.id AND e.kind='queued');
-- Older launched runs cannot always distinguish an initial queue from a later
-- requeue (launched_at is overwritten on retries). Keep ambiguous cases Started.
