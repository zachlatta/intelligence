-- One durable answer job per reviewer question. Commands use the existing host
-- stream and its idempotent create_agent executor; a retry retains old commands.
CREATE TABLE workflow_proposal_questions (
    question_id bigint PRIMARY KEY REFERENCES workflow_proposal_comments ON DELETE CASCADE,
    host text NOT NULL,
    command_id uuid REFERENCES commands ON DELETE SET NULL,
    answer_comment_id bigint UNIQUE REFERENCES workflow_proposal_comments,
    queued_at timestamptz NOT NULL
);
CREATE INDEX workflow_proposal_questions_command ON workflow_proposal_questions(command_id);
ALTER TABLE workflow_proposal_questions ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_proposal_questions FOR SELECT USING (
    EXISTS (SELECT 1 FROM workflow_proposal_comments c JOIN workflow_proposals p ON p.id=c.proposal_id
            WHERE c.id=question_id AND p.account_id=current_account_id())
);

-- Derive failures/expiry on read, including when no host is connected. A command
-- succeeding means the agent started, not that an answer has been received.
CREATE VIEW workflow_proposal_question_status WITH (security_invoker=true) AS
SELECT q.*, comment.proposal_id, coalesce(c.result->>'agentId','') AS agent_id,
    CASE
        WHEN q.answer_comment_id IS NOT NULL THEN 'answered'
        WHEN c.id IS NULL OR c.status IN ('failed','expired') THEN 'failed'
        WHEN c.status='succeeded' AND coalesce(c.result->>'agentId','')='' THEN 'failed'
        WHEN c.status='succeeded' AND c.finished_at + interval '30 minutes' <= now() THEN 'timed_out'
        WHEN c.status='succeeded' THEN 'answering'
        WHEN c.expires_at <= now() THEN 'failed'
        WHEN c.status='running' THEN 'starting'
        ELSE 'queued'
    END AS state,
    CASE
        WHEN q.answer_comment_id IS NOT NULL THEN ''
        WHEN c.id IS NULL THEN 'The dispatch command is no longer available. Retry on an enrolled host.'
        WHEN c.status IN ('failed','expired') THEN coalesce(nullif(c.error,''),'The agent could not start.')
        WHEN c.status='succeeded' AND coalesce(c.result->>'agentId','')='' THEN 'The host did not return an agent id.'
        WHEN c.status='succeeded' AND c.finished_at + interval '30 minutes' <= now() THEN 'The agent has not posted an answer within 30 minutes.'
        WHEN c.status<>'succeeded' AND c.expires_at <= now() THEN 'The host did not start the agent before the dispatch expired.'
        ELSE ''
    END AS error
FROM workflow_proposal_questions q
JOIN workflow_proposal_comments comment ON comment.id=q.question_id
LEFT JOIN commands c ON c.id=q.command_id;
