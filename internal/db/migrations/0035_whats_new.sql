-- What's New: Intelligence's release notes, and which of them each account has seen.
--
-- whats_new is synced from internal/whatsnew/entries.json each time the server starts
-- (whatsnew.Sync): one row per user-visible feature, newest first by released_on, then position
-- (its place in the file). It is the product's own notes, the same for every account, and holds
-- no account's state, so every account reads all of it.
CREATE TABLE whats_new (
    id          text PRIMARY KEY,
    released_on date NOT NULL,
    title       text NOT NULL,
    summary     text NOT NULL,
    media_url   text NOT NULL DEFAULT '', -- the demo (GIF, image or MP4) on this server; '' for none
    media_alt   text NOT NULL DEFAULT '',
    position    int NOT NULL
);
COMMENT ON TABLE whats_new IS 'Intelligence''s release notes, synced from internal/whatsnew/entries.json at server start';
ALTER TABLE whats_new ENABLE ROW LEVEL SECURITY;
CREATE POLICY every_account ON whats_new FOR SELECT USING (true);

-- Which entries an account has seen, written through POST /api/whats-new/seen.
CREATE TABLE whats_new_seen (
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    entry_id   text NOT NULL REFERENCES whats_new ON DELETE CASCADE,
    seen_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, entry_id)
);
COMMENT ON TABLE whats_new_seen IS 'the What''s New entries each account has seen';
ALTER TABLE whats_new_seen ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON whats_new_seen FOR SELECT USING (account_id = current_account_id());

-- The entries the reading account has not seen: what the top bar's What's New dot counts.
CREATE VIEW whats_new_unseen AS
SELECT w.*
FROM whats_new w
WHERE NOT EXISTS (SELECT 1 FROM whats_new_seen s WHERE s.entry_id = w.id AND s.account_id = current_account_id());
COMMENT ON VIEW whats_new_unseen IS 'What''s New entries the reading account has not seen';
ALTER VIEW whats_new_unseen SET (security_invoker = true);
