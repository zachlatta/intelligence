ALTER TABLE voice_memos ADD COLUMN recorded_at timestamptz;
ALTER TABLE voice_memos ADD COLUMN reason text NOT NULL DEFAULT 'Legacy receipt; reason not recorded';
UPDATE voice_memos SET reason = 'Started run ' || run_id::text WHERE decision = 'started' AND run_id IS NOT NULL;
CREATE INDEX voice_memos_recent ON voice_memos (account_id, decided_at DESC);
