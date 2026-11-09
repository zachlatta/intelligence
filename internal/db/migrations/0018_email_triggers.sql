-- Email triggers: an account connects one Gmail mailbox, and workflows whose trigger is email
-- start, steer or answer runs from what arrives in it (internal/email).

-- The account's mailbox. token is the Google refresh token, sealed (AES-GCM) with the server's
-- CONNECTIONS_KEY, which is only in the server's environment: reading it here gives nothing.
-- owners are the account holder's own addresses: only email from them (verified) answers a
-- run's question, and email from anyone else reaches a run marked as not theirs.
CREATE TABLE gmail_connections (
    account_id   bigint PRIMARY KEY REFERENCES accounts ON DELETE CASCADE,
    address      text NOT NULL,
    owners       text[] NOT NULL DEFAULT '{}',
    token        bytea NOT NULL,
    history_id   text NOT NULL,              -- Gmail's history id read up to
    backfilled   boolean NOT NULL DEFAULT false, -- the last 30 days are read in, for the editor's preview
    connected_at timestamptz NOT NULL,
    checked_at   timestamptz,                -- the last time the mailbox was read without error
    error        text NOT NULL DEFAULT '',   -- why the last read failed, if it did
    error_at     timestamptz
);

-- Every email the mailbox received (not what it sent, spam or trash), and what it did:
--   before:    it came before the mailbox was connected (read in for the preview); nothing happened
--   started:   it started a run of workflow_id
--   follow_up: it replied in the thread of a finished run, so it started a new one
--   added:     it went to the run of its thread, as a message to its orchestrator
--   answered:  it answered the question the run of its thread was waiting on
--   ignored:   nothing happened; reason says why
CREATE TABLE emails (
    id           bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    account_id   bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    gmail_id     text NOT NULL,
    thread_id    text NOT NULL,
    received_at  timestamptz NOT NULL,
    from_name    text NOT NULL DEFAULT '',
    from_address text NOT NULL,
    to_addresses text[] NOT NULL DEFAULT '{}',
    subject      text NOT NULL DEFAULT '',
    body         text NOT NULL DEFAULT '',
    attachments  text[] NOT NULL DEFAULT '{}',
    verified     boolean NOT NULL,           -- Gmail found the From domain sent it (DMARC)
    auth         text NOT NULL DEFAULT '',   -- why it is or is not verified
    bulk         boolean NOT NULL DEFAULT false,
    decision     text NOT NULL CHECK (decision IN ('before', 'started', 'follow_up', 'added', 'answered', 'ignored')),
    reason       text NOT NULL DEFAULT '',
    workflow_id  uuid REFERENCES workflows ON DELETE SET NULL,
    run_id       uuid REFERENCES workflow_runs ON DELETE SET NULL,
    decided_at   timestamptz NOT NULL,
    UNIQUE (account_id, gmail_id)
);
CREATE INDEX emails_received ON emails (account_id, received_at DESC);
CREATE INDEX emails_thread ON emails (account_id, thread_id) WHERE run_id IS NOT NULL;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['gmail_connections', 'emails'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY own_rows ON %I FOR SELECT USING (account_id = current_account_id())', t);
    END LOOP;
END $$;
