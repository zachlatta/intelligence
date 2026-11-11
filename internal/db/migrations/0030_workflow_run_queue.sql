-- A run started while its host is near capacity waits in a queue (status queued) and starts, oldest
-- first, once the host has room (internal/workflow/queue.go).
--   queue_reason: why it was queued, kept current while it waits and kept after it starts; '' never queued
--   dequeued_at:  when it left the queue, by itself or started by hand
--   launched_at:  when its first agent was asked for (queued or not); null for runs from before
--   launch_command: the command that asked for it; until the host has carried it out, the run
--                 counts as starting in its host's projected load
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_runs_status_check;
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_status_check
    CHECK (status IN ('queued', 'finding_project', 'starting', 'running', 'waiting', 'succeeded', 'failed', 'canceled'));
ALTER TABLE workflow_runs ADD COLUMN queue_reason text NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN dequeued_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN launched_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN launch_command uuid;
CREATE INDEX workflow_runs_queued ON workflow_runs (host, created_at) WHERE status = 'queued';

-- What one run adds to each host's load, learned from its last 3 days: every minute's CPU, memory
-- and load per CPU against how many runs were going then (launched, not finished), fitted with a
-- straight line (regr_slope); its slope is a run's footprint. Each metric is learned on its own
-- (cpu_learned, mem_learned, load_learned) when the fit can be trusted: at least 60 minutes, more
-- than one run count among them, a slope above zero (a run cannot lighten the host) and a fit that
-- explains at least a fifth of the metric's variation (regr_r2 >= 0.2). A learned footprint is kept
-- between a quarter and three times the default. Otherwise the default stands, deliberately
-- generous: 10% CPU, 5% memory and 0.15 load per CPU a run. The raw fits stay readable
-- (*_learned values, *_r2). The queue projects a host's load with it (internal/workflow/queue.go).
CREATE VIEW host_run_footprint WITH (security_invoker = true) AS
WITH runs AS ( -- the runs going at some point in the window
    SELECT host, launched_at, finished_at FROM workflow_runs
    WHERE launched_at IS NOT NULL AND (finished_at IS NULL OR finished_at > now() - interval '3 days')
), minutes AS (
    SELECT m.host, m.minute, m.cpu_avg_pct::float8 AS cpu_pct,
           CASE WHEN m.mem_total > 0 THEN 100 * (1 - m.mem_available_min::float8 / m.mem_total) END AS mem_pct,
           CASE WHEN m.cpus > 0 THEN m.load1::float8 / m.cpus END AS load_per_cpu,
           count(r.launched_at)::float8 AS runs
    FROM host_minutes m
    LEFT JOIN runs r ON r.host = m.host AND r.launched_at <= m.minute AND (r.finished_at IS NULL OR r.finished_at > m.minute)
    WHERE m.minute > now() - interval '3 days'
    GROUP BY m.host, m.minute, m.cpu_avg_pct, m.mem_total, m.mem_available_min, m.cpus, m.load1
), fit AS (
    SELECT host, count(*) AS minutes, count(DISTINCT runs) AS run_counts,
           regr_slope(cpu_pct, runs) AS cpu, regr_r2(cpu_pct, runs) AS cpu_r2,
           regr_slope(mem_pct, runs) AS mem, regr_r2(mem_pct, runs) AS mem_r2,
           regr_slope(load_per_cpu, runs) AS load, regr_r2(load_per_cpu, runs) AS load_r2
    FROM minutes GROUP BY host
), judged AS (
    SELECT h.name AS host, coalesce(f.minutes, 0) AS minutes, coalesce(f.run_counts, 0) AS run_counts,
           f.cpu, f.cpu_r2, f.mem, f.mem_r2, f.load, f.load_r2,
           coalesce(f.minutes >= 60 AND f.run_counts >= 2, false) AS enough
    FROM hosts h LEFT JOIN fit f ON f.host = h.name
), trusted AS (
    SELECT *, coalesce(enough AND cpu > 0 AND cpu_r2 >= 0.2, false) AS cpu_learned,
              coalesce(enough AND mem > 0 AND mem_r2 >= 0.2, false) AS mem_learned,
              coalesce(enough AND load > 0 AND load_r2 >= 0.2, false) AS load_learned
    FROM judged
)
SELECT host, minutes, run_counts, cpu_learned, mem_learned, load_learned,
       cpu AS cpu_pct_learned, cpu_r2, mem AS mem_pct_learned, mem_r2, load AS load_per_cpu_learned, load_r2,
       CASE WHEN cpu_learned THEN least(greatest(cpu, 2.5), 30) ELSE 10 END::float8 AS cpu_pct_per_run,
       CASE WHEN mem_learned THEN least(greatest(mem, 1.25), 15) ELSE 5 END::float8 AS mem_pct_per_run,
       CASE WHEN load_learned THEN least(greatest(load, 0.0375), 0.45) ELSE 0.15 END::float8 AS load_per_cpu_per_run
FROM trusted;

-- Each host's queue in the order its runs start: position 1 is next.
CREATE VIEW workflow_run_queue WITH (security_invoker = true) AS
SELECT id AS run_id, account_id, host, workflow_id, title,
       row_number() OVER (PARTITION BY host ORDER BY created_at, id) AS position,
       queue_reason, created_at AS queued_at
FROM workflow_runs
WHERE status = 'queued';
