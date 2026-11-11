-- Sum concurrent agents before taking a run's peak: a run includes its workers.
CREATE VIEW workflow_run_memory WITH (security_invoker = true) AS
SELECT run_id, max(memory_bytes)::bigint AS peak_memory_bytes
FROM (
    SELECT h.run_id, m.minute, sum(m.mem_max) AS memory_bytes
    FROM workflow_run_agents h JOIN workflow_runs r ON r.id = h.run_id
    JOIN agent_minutes m ON m.host = h.host AND m.agent_id = h.agent_id
    WHERE m.minute >= date_trunc('minute', r.created_at)
      AND (r.finished_at IS NULL OR m.minute <= r.finished_at)
    GROUP BY h.run_id, m.minute
) minutes GROUP BY run_id;

-- Repository roots are host-specific. Do not mix unrelated repositories with the same name.
CREATE VIEW workflow_repository_memory WITH (security_invoker = true) AS
SELECT r.account_id, r.host, r.repo, count(*) AS observed_runs,
    greatest(1073741824, ceil(max(m.peak_memory_bytes) * 1.25))::bigint AS estimated_memory_bytes
FROM workflow_runs r JOIN workflow_run_memory m ON m.run_id = r.id
WHERE r.finished_at IS NOT NULL AND m.peak_memory_bytes > 0
GROUP BY r.account_id, r.host, r.repo;
ALTER TABLE workflow_runs ADD COLUMN estimated_memory_bytes bigint NOT NULL DEFAULT 4294967296 CHECK (estimated_memory_bytes > 0);
-- Distinguish a finder still to run from a selected `none` project waiting for capacity.
ALTER TABLE workflow_runs ADD COLUMN project_selected boolean NOT NULL DEFAULT false;
UPDATE workflow_runs r SET project_selected = true
FROM workflow_versions v WHERE v.workflow_id = r.workflow_id AND v.version = r.version
    AND (v.definition->'project'->>'mode' = 'fixed' OR r.status IN ('starting','running','waiting','succeeded','failed','canceled'));
UPDATE workflow_runs r SET repo = v.definition->'project'->>'directory'
FROM workflow_versions v WHERE v.workflow_id = r.workflow_id AND v.version = r.version
    AND v.definition->'project'->>'mode' = 'fixed' AND r.repo = '';

UPDATE workflow_runs r SET estimated_memory_bytes = m.estimated_memory_bytes
FROM workflow_repository_memory m WHERE r.project_selected
    AND m.account_id = r.account_id AND m.host = r.host AND m.repo = r.repo;
