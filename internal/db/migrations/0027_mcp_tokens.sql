-- Dedicated read-only MCP credentials. Only the hash is retained; browser sessions and host
-- keys are deliberately not accepted as MCP credentials.
CREATE TABLE mcp_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX mcp_tokens_account ON mcp_tokens (account_id);
ALTER TABLE mcp_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON mcp_tokens FOR SELECT USING (account_id = current_account_id());
