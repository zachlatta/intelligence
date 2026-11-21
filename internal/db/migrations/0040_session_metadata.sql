ALTER TABLE sessions
    ADD COLUMN user_agent text NOT NULL DEFAULT '',
    ADD COLUMN created_from_path text NOT NULL DEFAULT '';

COMMENT ON COLUMN sessions.user_agent IS 'browser User-Agent at session creation, limited to 1024 bytes';
COMMENT ON COLUMN sessions.created_from_path IS 'same-origin page path used to start the session, without query parameters or fragments; empty for older sessions';
