-- When PDW's voice memo feed is next read, and how many reads in a row have failed (for backoff).
-- Kept here, not in memory, so every server instance shares one schedule. Existing RLS applies.
ALTER TABLE voice_memo_polling
    ADD COLUMN next_poll_at timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    ADD COLUMN retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count >= 0);
