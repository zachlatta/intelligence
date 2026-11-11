CREATE TABLE workflow_proposal_comments (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    proposal_id uuid NOT NULL REFERENCES workflow_proposals ON DELETE CASCADE,
    author text NOT NULL,
    text text NOT NULL CHECK (length(btrim(text)) > 0 AND octet_length(text) <= 20000),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workflow_proposal_comments_thread ON workflow_proposal_comments (proposal_id, id);
ALTER TABLE workflow_proposal_comments ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_proposal_comments FOR SELECT USING (
    EXISTS (SELECT 1 FROM workflow_proposals p WHERE p.id = proposal_id AND p.account_id = current_account_id())
);
