-- Temporary secrets Zach gives a workflow run on an ask (a password, an API key), stored by
-- Intelligence itself. The value is kept only as AES-256-GCM ciphertext sealed with the server's
-- INTELLIGENCE_SECRETS_KEY and bound to its row (id, run, account), so read-only SQL sees bytes
-- it cannot open. The ciphertext is wiped (the row's metadata kept) when the secret expires, when
-- Zach deletes it, and when its run ends.

CREATE TABLE workflow_run_secrets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    account_id   bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ask_event_id bigint NOT NULL REFERENCES workflow_run_events(id) ON DELETE CASCADE,
    position     integer NOT NULL DEFAULT 0,
    name         text NOT NULL,
    purpose      text NOT NULL DEFAULT '',
    username     text NOT NULL DEFAULT '',
    multiline    boolean NOT NULL DEFAULT false,
    -- pending: asked, not yet given; stored: ciphertext held; dismissed: Zach replied without it;
    -- purged: ciphertext wiped (purge_reason says why).
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'stored', 'dismissed', 'purged')),
    ciphertext   bytea,
    stored_by    text NOT NULL DEFAULT '',
    stored_at    timestamptz,
    expires_at   timestamptz,
    purged_at    timestamptz,
    purge_reason text NOT NULL DEFAULT '' CHECK (purge_reason IN ('', 'expired', 'deleted', 'run_ended')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'stored') = (ciphertext IS NOT NULL)),
    CHECK (status <> 'stored' OR (stored_at IS NOT NULL AND expires_at IS NOT NULL))
);

CREATE INDEX workflow_run_secrets_run ON workflow_run_secrets (run_id, ask_event_id, position);
CREATE INDEX workflow_run_secrets_account ON workflow_run_secrets (account_id, created_at DESC);
CREATE INDEX workflow_run_secrets_expiry ON workflow_run_secrets (expires_at) WHERE status = 'stored';

ALTER TABLE workflow_run_secrets ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_secrets FOR SELECT
    USING (account_id = current_account_id());

-- Each time an agent fetched a stored secret: who (the host), when and which. Never the value.
CREATE TABLE workflow_run_secret_fetches (
    id         bigserial PRIMARY KEY,
    secret_id  uuid NOT NULL REFERENCES workflow_run_secrets(id) ON DELETE CASCADE,
    run_id     uuid NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    host       text NOT NULL,
    at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workflow_run_secret_fetches_secret ON workflow_run_secret_fetches (secret_id, at DESC);
CREATE INDEX workflow_run_secret_fetches_run ON workflow_run_secret_fetches (run_id, at DESC);

ALTER TABLE workflow_run_secret_fetches ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_secret_fetches FOR SELECT
    USING (account_id = current_account_id());

-- A run that ends (whatever path ends it) wipes its secrets in the same transaction.
CREATE FUNCTION workflow_run_secrets_purge_on_end() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE workflow_run_secrets
       SET status = 'purged', ciphertext = NULL, purged_at = now(), purge_reason = 'run_ended'
     WHERE run_id = NEW.id AND status = 'stored';
    UPDATE workflow_run_secrets SET status = 'dismissed'
     WHERE run_id = NEW.id AND status = 'pending';
    RETURN NEW;
END;
$$;

CREATE TRIGGER workflow_run_secrets_purge_on_end
    AFTER UPDATE OF status ON workflow_runs
    FOR EACH ROW
    WHEN (NEW.status IN ('succeeded', 'failed', 'canceled') AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION workflow_run_secrets_purge_on_end();
