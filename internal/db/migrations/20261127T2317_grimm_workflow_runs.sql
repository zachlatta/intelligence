-- Grimm workflow run requests: links a Grimm session to a workflow run proposal
-- (workflow_proposals with operation = 'run') and tracks the last reported state
-- so the Grimm supervisor can wake the manager on run milestones without duplicate notices.

CREATE TABLE grimm_workflow_runs (
    proposal_id          uuid PRIMARY KEY REFERENCES workflow_proposals(id) ON DELETE CASCADE,
    session_id           uuid NOT NULL REFERENCES grimm_sessions(id) ON DELETE CASCADE,
    account_id           bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    requested_by         text NOT NULL DEFAULT '',
    message_id           bigint REFERENCES grimm_messages(id) ON DELETE SET NULL,
    last_proposal_status text NOT NULL DEFAULT 'pending',
    last_run_status      text NOT NULL DEFAULT '',
    last_run_step        integer NOT NULL DEFAULT -1,
    last_run_note        text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX grimm_workflow_runs_session ON grimm_workflow_runs (session_id, created_at);

ALTER TABLE grimm_workflow_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON grimm_workflow_runs FOR SELECT USING (account_id = current_account_id());

-- Live status of every workflow run requested in a Grimm session: proposal state,
-- workflow metadata, live run status, current step info, waiting/sign-off info,
-- and per-step/gate outcomes from workflow_run_events.
CREATE VIEW grimm_workflow_run_status WITH (security_invoker = true) AS
SELECT g.proposal_id,
       g.session_id,
       g.account_id,
       g.requested_by,
       g.message_id,
       g.created_at,
       p.workflow_id,
       coalesce(w.name, p.before_state->>'name', '') AS workflow_name,
       coalesce(r.host, p.before_state->'definition'->>'host', '') AS host,
       p.summary,
       p.input,
       p.effective_status AS proposal_status,
       p.decided_at,
       p.expires_at,
       p.withdraw_reason,
       p.started_run_id AS run_id,
       coalesce(r.status, '') AS run_status,
       coalesce(r.step, -1) AS run_step,
       coalesce(r.title, '') AS run_title,
       coalesce(r.note, '') AS run_note,
       coalesce(r.error, '') AS run_error,
       coalesce(r.activity_waiting_on, '') AS waiting_on,
       r.created_at AS run_created_at,
       r.updated_at AS run_updated_at,
       r.finished_at AS run_finished_at,
       coalesce(jsonb_array_length(v.definition->'steps'), 0) AS step_count,
       coalesce(v.definition->'steps'->r.step->>'id', '') AS step_id,
       coalesce(v.definition->'steps'->r.step->>'title', '') AS step_title,
       coalesce(v.definition->'steps'->r.step->>'kind', '') AS step_kind,
       coalesce((v.definition->'steps'->r.step->>'requireHumanSignoff')::boolean, false) AS require_human_signoff,
       coalesce(
           (SELECT jsonb_agg(
               jsonb_build_object(
                   'index', (s.ord - 1)::int,
                   'id', coalesce(s.elem->>'id', ''),
                   'title', coalesce(s.elem->>'title', ''),
                   'kind', coalesce(s.elem->>'kind', 'step'),
                   'outcome', CASE
                       WHEN r.id IS NULL THEN 'pending'
                       WHEN r.status = 'succeeded' THEN 'passed'
                       WHEN (s.ord - 1) < r.step THEN
                           coalesce((SELECT CASE e.kind WHEN 'skip' THEN 'skipped' ELSE 'passed' END
                                     FROM workflow_run_events e
                                     WHERE e.run_id = r.id AND e.step = (s.ord - 1) AND e.kind IN ('next', 'skip', 'done')
                                     ORDER BY e.id DESC LIMIT 1), 'passed')
                       WHEN (s.ord - 1) = r.step THEN
                           CASE
                               WHEN r.status = 'waiting' THEN 'waiting'
                               WHEN r.status = 'failed' THEN 'failed'
                               WHEN r.status = 'canceled' THEN 'canceled'
                               WHEN r.status = 'stalled' THEN 'stalled'
                               ELSE 'current'
                           END
                       ELSE 'pending'
                   END
               ) ORDER BY s.ord
           )
           FROM jsonb_array_elements(coalesce(v.definition->'steps', '[]'::jsonb)) WITH ORDINALITY AS s(elem, ord)),
           '[]'::jsonb
       ) AS steps
FROM grimm_workflow_runs g
JOIN workflow_proposal_status p ON p.id = g.proposal_id
LEFT JOIN workflows w ON w.id = p.workflow_id
LEFT JOIN workflow_runs r ON r.id = p.started_run_id
LEFT JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version;
