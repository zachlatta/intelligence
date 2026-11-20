-- Zach's input to a run's orchestrator (his reply to its ask, or later input such as an email in
-- the run's thread) and every message that carried it there (internal/workflow/inputs.go).
--
-- A host that has lost its Paseo daemon fails a message it is sent, so a reply recorded while the
-- host was down never reached the orchestrator, which then asked the same question again (run
-- 2c75da8c, 2026-11-19). An input whose every carrier failed or expired is undelivered: the next
-- message to the orchestrator (a watchdog continuation, Resume, more input) carries it again,
-- verbatim, and an ask made while a reply is undelivered gets the reply back instead of going to Zach.
--
--   original    the send_message made for the input
--   redelivery  a later send_message that carried it again
--   ask         the answer to an ask that carried it (no command: the orchestrator read it at once)
CREATE TABLE workflow_run_input_deliveries (
 run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 event_id bigint NOT NULL REFERENCES workflow_run_events ON DELETE CASCADE,
 via text NOT NULL CHECK (via IN ('original','redelivery','ask')),
 command_id uuid REFERENCES commands ON DELETE SET NULL,
 created_at timestamptz NOT NULL,
 UNIQUE (event_id, command_id)
);
CREATE INDEX workflow_run_input_deliveries_run ON workflow_run_input_deliveries (run_id, event_id);
ALTER TABLE workflow_run_input_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_input_deliveries FOR SELECT USING (account_id=current_account_id());

-- One row per input: delivered once a carrier succeeded (or an ask's answer carried it; a carrier
-- whose command row is gone counts as delivered), pending while one may still arrive (unfinished
-- and not yet past its expiry).
CREATE VIEW workflow_run_inputs WITH (security_invoker = true) AS
SELECT o.run_id, o.account_id, o.event_id, e.at, e.kind, o.command_id,
 EXISTS (SELECT 1 FROM workflow_run_input_deliveries d LEFT JOIN commands c ON c.id=d.command_id
   WHERE d.event_id=o.event_id AND (d.command_id IS NULL OR c.status='succeeded')) AS delivered,
 EXISTS (SELECT 1 FROM workflow_run_input_deliveries d JOIN commands c ON c.id=d.command_id
   WHERE d.event_id=o.event_id AND c.status IN ('queued','sent','running') AND c.expires_at>now()) AS pending,
 (SELECT count(*) FROM workflow_run_input_deliveries d WHERE d.event_id=o.event_id AND d.via<>'original') AS redeliveries
FROM workflow_run_input_deliveries o JOIN workflow_run_events e ON e.id=o.event_id
WHERE o.via='original';

-- Live runs' earlier inputs: the send_message to the orchestrator made with the event (Reply and
-- MessageTx record the event just after queueing it, in one transaction).
INSERT INTO workflow_run_input_deliveries (run_id, account_id, event_id, via, command_id, created_at)
SELECT e.run_id, e.account_id, e.id, 'original', c.id, e.at
FROM workflow_run_events e
JOIN workflow_runs r ON r.id=e.run_id
JOIN LATERAL (
 SELECT c.id FROM commands c
 WHERE c.host=r.host AND c.kind='send_message' AND c.args->>'agentId'=r.orchestrator_id
   AND c.created_at BETWEEN e.at - interval '5 seconds' AND e.at + interval '5 seconds'
   AND CASE WHEN e.kind='reply' THEN strpos(c.args->>'text', e.text) > 0
            ELSE strpos(c.args->>'text', e.text || ':') = 1 END
 ORDER BY abs(extract(epoch FROM c.created_at - e.at)) LIMIT 1) c ON true
WHERE e.kind IN ('reply','message') AND e.text <> '' AND r.orchestrator_id <> ''
  AND r.status IN ('running','waiting','stalled');
