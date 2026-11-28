-- Grimm workflow orchestration: attribute Grimm-originated run events to their session,
-- track richer run milestones (queue reason, gate send-backs), and expose ask_kind,
-- deliverables, done summaries, and Grimm guidance events on grimm_workflow_run_status.

ALTER TABLE workflow_run_events
    ADD COLUMN grimm_session_id uuid REFERENCES grimm_sessions(id) ON DELETE SET NULL;

CREATE INDEX workflow_run_events_grimm_session
    ON workflow_run_events (run_id, grimm_session_id)
    WHERE grimm_session_id IS NOT NULL;

ALTER TABLE grimm_workflow_runs
    ADD COLUMN last_queue_reason text NOT NULL DEFAULT '',
    ADD COLUMN last_back_event_id bigint NOT NULL DEFAULT 0;

CREATE OR REPLACE VIEW grimm_workflow_run_status WITH (security_invoker = true) AS
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
       ) AS steps,
       coalesce(r.queue_reason, '') AS queue_reason,
       CASE
           WHEN r.id IS NULL OR r.status <> 'waiting' THEN ''
           WHEN coalesce(v.definition->'steps'->r.step->>'kind', '') = 'signoff'
                OR coalesce((v.definition->'steps'->r.step->>'requireHumanSignoff')::boolean, false) THEN 'signoff'
           WHEN EXISTS (
               SELECT 1 FROM workflow_run_secrets sec
               WHERE sec.run_id = r.id
                 AND sec.status = 'pending'
                 AND sec.ask_event_id = (
                     SELECT coalesce(max(e.id), 0)
                     FROM workflow_run_events e
                     WHERE e.run_id = r.id AND e.kind = 'ask'
                 )
           ) THEN 'secret'
           ELSE 'clarification'
       END AS ask_kind,
       coalesce(
           (SELECT e.id FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind = 'back' ORDER BY e.id DESC LIMIT 1),
           0
       ) AS last_back_event_id,
       coalesce(
           (SELECT e.text FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind = 'back' ORDER BY e.id DESC LIMIT 1),
           ''
       ) AS last_back_reason,
       coalesce(
           (SELECT e.step FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind = 'back' ORDER BY e.id DESC LIMIT 1),
           -1
       ) AS last_back_step,
       coalesce(
           (SELECT e.to_step FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind = 'back' ORDER BY e.id DESC LIMIT 1),
           -1
       ) AS last_back_to_step,
       CASE
           WHEN r.status = 'succeeded' THEN
               coalesce(
                   (SELECT e.text FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind IN ('done', 'next') ORDER BY e.id DESC LIMIT 1),
                   ''
               )
           ELSE ''
       END AS done_summary,
       coalesce(
           (SELECT jsonb_agg(e.text ORDER BY e.id) FROM workflow_run_events e WHERE e.run_id = r.id AND e.kind = 'claim' AND e.text <> ''),
           '[]'::jsonb
       ) AS deliverables,
       coalesce(
           (SELECT jsonb_agg(
               jsonb_build_object(
                   'id', e.id,
                   'kind', e.kind,
                   'step', e.step,
                   'text', e.text,
                   'at', e.at
               ) ORDER BY e.id
           )
           FROM workflow_run_events e
           WHERE e.run_id = r.id
             AND e.grimm_session_id = g.session_id
             AND e.kind IN ('input', 'reply', 'cancel')),
           '[]'::jsonb
       ) AS grimm_events
FROM grimm_workflow_runs g
JOIN workflow_proposal_status p ON p.id = g.proposal_id
LEFT JOIN workflows w ON w.id = p.workflow_id
LEFT JOIN workflow_runs r ON r.id = p.started_run_id
LEFT JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version;
