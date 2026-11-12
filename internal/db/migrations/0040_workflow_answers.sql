ALTER TABLE workflow_runs ADD COLUMN requested_at timestamptz NOT NULL DEFAULT now();
-- Best effort for historical inputs. Malformed dates never prevent the upgrade.
DO $$
DECLARE r record; stamp timestamptz; raw text;
BEGIN
 FOR r IN SELECT id,input,created_at FROM workflow_runs LOOP
  stamp := r.created_at;
  raw := substring(r.input from '(?m)^(?:Date:|Recorded:) ([^\r\n]+)');
  BEGIN
   IF raw IS NOT NULL THEN stamp := raw::timestamptz; END IF;
   IF NOT isfinite(stamp) THEN stamp := r.created_at; END IF;
  EXCEPTION WHEN OTHERS THEN stamp := r.created_at;
  END;
  UPDATE workflow_runs SET requested_at=stamp WHERE id=r.id;
 END LOOP;
END $$;

CREATE TABLE workflow_run_answers (
 id bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
 run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
 account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
 channel text NOT NULL CHECK (length(channel) BETWEEN 1 AND 80),
 message_id text NOT NULL CHECK (length(message_id) BETWEEN 1 AND 500),
 thread_id text NOT NULL DEFAULT '' CHECK (length(thread_id) <= 500),
 sent_at timestamptz NOT NULL,
 checked boolean NOT NULL DEFAULT false,
 UNIQUE (run_id,channel,message_id)
);
ALTER TABLE workflow_run_answers ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON workflow_run_answers FOR SELECT USING (account_id=current_account_id());
CREATE VIEW workflow_runs_unanswered WITH (security_invoker=true) AS
 SELECT r.*, extract(epoch FROM (now()-r.requested_at))/60 AS waiting_minutes
 FROM workflow_runs r WHERE requested_at < now()-interval '30 minutes'
 AND NOT EXISTS (SELECT 1 FROM workflow_run_answers a WHERE a.run_id=r.id);
