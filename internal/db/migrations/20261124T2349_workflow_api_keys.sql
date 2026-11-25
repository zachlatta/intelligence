-- Scoped API keys for API-triggered workflows.
--
-- A key is scoped to one workflow of its account. Only its SHA-256 hash is stored
-- (the raw secret has 256 bits of entropy, like host_keys and mcp_tokens). Revocation
-- is recorded with revoked_at so the workflow page and SQL keep an audit trail.
CREATE TABLE workflow_api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    workflow_id  uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    name         text NOT NULL,
    key_prefix   text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX workflow_api_keys_workflow ON workflow_api_keys (workflow_id, created_at DESC, id DESC);
CREATE INDEX workflow_api_keys_account ON workflow_api_keys (account_id);

ALTER TABLE workflow_api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_api_keys FOR SELECT
    USING (account_id = current_account_id());

-- Record which API key triggered a run and support idempotent API triggers.
ALTER TABLE workflow_runs
    ADD COLUMN api_key_id uuid REFERENCES workflow_api_keys(id) ON DELETE SET NULL,
    ADD COLUMN idempotency_key text NOT NULL DEFAULT '',
    ADD COLUMN idempotency_hash text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX workflow_runs_api_idempotency
    ON workflow_runs (workflow_id, idempotency_key)
    WHERE idempotency_key <> '';

CREATE INDEX workflow_runs_workflow_created
    ON workflow_runs (workflow_id, created_at DESC, id DESC);

-- Record which API key caused a run event (reply, input, cancel, resume, start, retry).
ALTER TABLE workflow_run_events
    ADD COLUMN api_key_id uuid REFERENCES workflow_api_keys(id) ON DELETE SET NULL;

