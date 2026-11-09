-- Canonical image bytes and metadata. Host files are disposable delivery copies.
CREATE TABLE attachments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    name text NOT NULL,
    media_type text NOT NULL CHECK (media_type IN ('image/png', 'image/jpeg', 'image/gif')),
    data bytea NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 4194304),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_account ON attachments(account_id);
ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON attachments FOR SELECT USING (account_id = current_account_id());
