-- Durable poll, quota pacing and retry deadlines. Existing account RLS applies.
ALTER TABLE gmail_connections
    ADD COLUMN next_poll_at timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    ADD COLUMN next_request_at timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    ADD COLUMN backfill_ids text[], -- remaining preview messages; NULL before the first search
    ADD COLUMN retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count >= 0);
