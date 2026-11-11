-- The credential stays in the environment, bound to one account. No full transcripts are stored.
CREATE TABLE voice_memo_polling (
 account_id bigint PRIMARY KEY REFERENCES accounts ON DELETE CASCADE,
 cursor text NOT NULL DEFAULT '',
 initialized boolean NOT NULL DEFAULT false,
 enabled_at timestamptz NOT NULL DEFAULT now(),
 checked_at timestamptz,
 error text NOT NULL DEFAULT ''
);
CREATE TABLE voice_memos (
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 source text NOT NULL,
 pdw_account text NOT NULL,
 recording_id text NOT NULL,
 title text NOT NULL,
 preview text NOT NULL CHECK (char_length(preview) <= 1000),
 transcript_length integer NOT NULL,
 decision text NOT NULL CHECK (decision IN ('started','ignored')),
 workflow_id uuid REFERENCES workflows,
 run_id uuid REFERENCES workflow_runs,
 decided_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (account_id,source,pdw_account,recording_id)
);
ALTER TABLE voice_memo_polling ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON voice_memo_polling FOR SELECT USING (account_id = current_account_id());
ALTER TABLE voice_memos ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON voice_memos FOR SELECT USING (account_id = current_account_id());
