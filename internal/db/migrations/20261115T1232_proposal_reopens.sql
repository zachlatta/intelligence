CREATE TABLE workflow_proposal_reopens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    proposal_id uuid NOT NULL REFERENCES workflow_proposals ON DELETE CASCADE,
    actor text NOT NULL,
    question_id bigint REFERENCES workflow_proposal_questions,
    rejected_at timestamptz NOT NULL,
    reopened_at timestamptz NOT NULL
);
ALTER TABLE workflow_proposal_reopens ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_proposal_reopens FOR SELECT USING (
    EXISTS (SELECT 1 FROM workflow_proposals p WHERE p.id=proposal_id AND p.account_id=current_account_id())
);
COMMENT ON TABLE workflow_proposal_reopens IS 'Reopening history, including the prior rejection and authorizing reviewer question';
