ALTER TABLE workflow_proposals
    ADD COLUMN proposed_by text NOT NULL DEFAULT '',
    ADD COLUMN run_id text NOT NULL DEFAULT '',
    ADD COLUMN withdraw_reason text NOT NULL DEFAULT '',
    ADD COLUMN replaces_id uuid REFERENCES workflow_proposals;

-- Recreate so the new columns are included in this SQL discovery surface.
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
