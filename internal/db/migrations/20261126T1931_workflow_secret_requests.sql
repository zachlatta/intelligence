-- Non-secret metadata for secrets requested on a workflow run's ask.
-- Secret values are written directly to Vaultwarden and are NEVER stored in Postgres.

CREATE TABLE workflow_run_secret_requests (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id            uuid NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    account_id        bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ask_event_id      bigint NOT NULL REFERENCES workflow_run_events(id) ON DELETE CASCADE,
    position          integer NOT NULL DEFAULT 0,
    name              text NOT NULL,
    purpose           text NOT NULL DEFAULT '',
    username          text NOT NULL DEFAULT '',
    target_collection text NOT NULL DEFAULT '',
    status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'stored', 'dismissed')),
    vault_item_id     text NOT NULL DEFAULT '',
    vault_item_name   text NOT NULL DEFAULT '',
    stored_by         text NOT NULL DEFAULT '',
    stored_at         timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workflow_run_secret_requests_run ON workflow_run_secret_requests (run_id, ask_event_id, position);
CREATE INDEX workflow_run_secret_requests_account ON workflow_run_secret_requests (account_id, created_at DESC);

ALTER TABLE workflow_run_secret_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_secret_requests FOR SELECT
    USING (account_id = current_account_id());
