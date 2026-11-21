ALTER TABLE sessions
    ADD COLUMN user_agent text NOT NULL DEFAULT '',
    ADD COLUMN created_from_path text NOT NULL DEFAULT '';

COMMENT ON COLUMN sessions.user_agent IS 'browser User-Agent at session creation, limited to 1024 bytes';
COMMENT ON COLUMN sessions.created_from_path IS 'API route path that issued the session, without query parameters; empty for older sessions';
