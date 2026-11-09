-- Audit survives agent/workspace replacement and removal. Do not store tool
-- inputs: prompts may contain credentials; identity and tool name suffice.
CREATE TABLE permission_resolutions (
 host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
 agent_id text NOT NULL,
 permission_id text NOT NULL,
 provider text NOT NULL,
 mode text NOT NULL,
 tool text NOT NULL,
 status text NOT NULL CHECK (status IN ('pending','denied','error')),
 message text NOT NULL,
 error text NOT NULL DEFAULT '',
 attempted_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL,
 PRIMARY KEY(host,agent_id,permission_id)
);
ALTER TABLE permission_resolutions ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON permission_resolutions FOR SELECT
 USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));
