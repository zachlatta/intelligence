-- How long a workflow run spent on each step, from the moves in workflow_run_events.
--
-- A run enters step 0 when its orchestrator starts (`workspace`), and the step a `next`, `skip`
-- or `back` goes to (`to_step`). It leaves the step at its next move: `next`, `skip`, `back`,
-- `done`, `fail` or `cancel`. Time waiting on Zach's reply counts toward the step it was asked in.

-- One row per time a run was on a step: visit is 1 the first time, 2 when it came back, and so
-- on. ended_at and ended_by (the move that left it) are null while the run is still on it.
CREATE VIEW workflow_run_step_visits WITH (security_invoker = true) AS
WITH moves AS (
    SELECT run_id, account_id, id, at,
        CASE WHEN kind = 'workspace' THEN 0 WHEN kind IN ('next', 'skip', 'back') THEN to_step END AS entered,
        lead(at) OVER w AS left_at,
        lead(kind) OVER w AS left_by
    FROM workflow_run_events
    WHERE kind IN ('workspace', 'next', 'skip', 'back', 'done', 'fail', 'cancel')
    WINDOW w AS (PARTITION BY run_id ORDER BY id)
)
SELECT run_id, account_id, entered AS step,
    row_number() OVER (PARTITION BY run_id, entered ORDER BY id)::integer AS visit,
    at AS started_at, left_at AS ended_at, left_by AS ended_by
FROM moves
WHERE entered IS NOT NULL;

-- One row per step a run has been on: its id and title in the run's version, how many visits,
-- and the time spent on it over all of them. current: the run is on it now, so time_spent runs
-- to now() (the query's transaction time).
CREATE VIEW workflow_run_step_times WITH (security_invoker = true) AS
SELECT s.run_id, s.account_id, s.step,
    coalesce(v.definition -> 'steps' -> s.step ->> 'id', '') AS step_id,
    coalesce(v.definition -> 'steps' -> s.step ->> 'title', '') AS title,
    count(*)::integer AS visits,
    sum(coalesce(s.ended_at, now()) - s.started_at) AS time_spent,
    bool_or(s.ended_at IS NULL) AS current
FROM workflow_run_step_visits s
JOIN workflow_runs r ON r.id = s.run_id
JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version
GROUP BY s.run_id, s.account_id, s.step, v.definition;
