CREATE TABLE workflow_proposals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    proposer text NOT NULL,
    operation text NOT NULL CHECK (operation IN ('create', 'update', 'delete')),
    workflow_id uuid REFERENCES workflows,
    summary text NOT NULL,
    before_state jsonb,
    proposed_state jsonb,
    after_state jsonb,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied', 'rejected', 'withdrawn', 'expired', 'stale', 'undone')),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    decided_at timestamptz,
    undone_at timestamptz,
    undo_state jsonb
);
CREATE INDEX workflow_proposals_account ON workflow_proposals (account_id, created_at DESC);
ALTER TABLE workflow_proposals ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_proposals FOR SELECT USING (account_id = current_account_id());

-- Expiry and drift are visible immediately, without a background worker or a write on read.
CREATE VIEW workflow_proposal_status WITH (security_invoker = true) AS
SELECT p.*, CASE
    WHEN p.status <> 'pending' THEN p.status
    WHEN p.expires_at <= now() THEN 'expired'
    WHEN p.operation <> 'create' AND p.before_state IS DISTINCT FROM
        jsonb_build_object('name', w.name, 'description', w.description,
          'version', w.version, 'isDefault', w.is_default,
          'deleted', w.deleted_at IS NOT NULL, 'definition', v.definition) THEN 'stale'
    ELSE 'pending'
END AS effective_status
FROM workflow_proposals p
LEFT JOIN workflows w ON w.id = p.workflow_id
LEFT JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version;
