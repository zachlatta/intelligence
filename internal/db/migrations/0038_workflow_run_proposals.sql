ALTER TABLE workflow_proposals DROP CONSTRAINT workflow_proposals_operation_check;
ALTER TABLE workflow_proposals ADD CONSTRAINT workflow_proposals_operation_check
    CHECK (operation IN ('create', 'update', 'delete', 'run'));
ALTER TABLE workflow_proposals ADD COLUMN input text NOT NULL DEFAULT '';
ALTER TABLE workflow_proposals ADD COLUMN started_run_id uuid REFERENCES workflow_runs ON DELETE SET NULL;
COMMENT ON COLUMN workflow_proposals.input IS 'Full task input for an approval-gated workflow run';
COMMENT ON COLUMN workflow_proposals.started_run_id IS 'Run created atomically on browser approval, distinct from proposer run_id attribution';
-- Recreate to include the new columns while retaining expiry/drift and invoker security.
DROP VIEW workflow_proposal_status;
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
