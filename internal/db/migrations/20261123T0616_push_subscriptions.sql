-- Web Push subscriptions (PWA notifications).
--
-- subscription_sealed holds the browser PushSubscription JSON ({endpoint, p256dh, auth}) sealed
-- with AES-256-GCM using a key derived from VAPID_PRIVATE_KEY, so reading this table through
-- /api/sql exposes device metadata without revealing push endpoints or encryption keys.

CREATE TABLE push_subscriptions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id          bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    endpoint_hash       text NOT NULL,
    endpoint_host       text NOT NULL DEFAULT '',
    user_agent          text NOT NULL DEFAULT '',
    subscription_sealed bytea NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_sent_at        timestamptz,
    last_error          text NOT NULL DEFAULT '',
    last_error_at       timestamptz,
    UNIQUE (account_id, endpoint_hash)
);

CREATE INDEX push_subscriptions_account ON push_subscriptions (account_id, created_at DESC);

ALTER TABLE push_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON push_subscriptions FOR SELECT
    USING (account_id = current_account_id());
