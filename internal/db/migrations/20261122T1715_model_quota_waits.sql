-- Runs waiting for an eligible model in a model group, and host-level outage alerts.
--
-- A model-availability block ("No eligible model in <group>") is a wait state
-- (activity_waiting_on = 'model quota'), not an ask: Intelligence resumes each blocked run
-- automatically when an eligible model returns, and sends at most one host-level notice
-- (model_quota_alerts) if the wait passes the outage threshold (30 minutes).
CREATE TABLE workflow_model_waits (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
    host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
    step integer NOT NULL,
    group_id uuid NOT NULL REFERENCES model_groups,
    token text NOT NULL,
    since timestamptz NOT NULL,
    last_checked_at timestamptz NOT NULL,
    resolved_at timestamptz,
    resolved_reason text NOT NULL DEFAULT '',
    command_id uuid REFERENCES commands ON DELETE SET NULL
);
CREATE UNIQUE INDEX workflow_model_waits_open ON workflow_model_waits (run_id) WHERE resolved_at IS NULL;
CREATE INDEX workflow_model_waits_host_open ON workflow_model_waits (account_id, host) WHERE resolved_at IS NULL;

CREATE TABLE model_quota_alerts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
    since timestamptz NOT NULL,
    opened_at timestamptz NOT NULL,
    tokens text[] NOT NULL DEFAULT '{}',
    blocked_runs integer NOT NULL,
    run_ids uuid[] NOT NULL DEFAULT '{}',
    notify boolean NOT NULL,
    alert_text text NOT NULL DEFAULT '',
    alert_sent_at timestamptz,
    alert_attempts integer NOT NULL DEFAULT 0,
    alert_error text NOT NULL DEFAULT '',
    recovered_at timestamptz
);
CREATE UNIQUE INDEX model_quota_alerts_open ON model_quota_alerts (account_id, host) WHERE recovered_at IS NULL;
CREATE INDEX model_quota_alerts_opened ON model_quota_alerts (opened_at DESC);

DO $$ DECLARE t text; BEGIN
    FOREACH t IN ARRAY ARRAY['workflow_model_waits', 'model_quota_alerts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY own_rows ON %I FOR SELECT USING (account_id = current_account_id())', t);
    END LOOP;
END $$;
