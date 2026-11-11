-- A committed marker prevents a repeat open when Gmail delivers an email again. At most once:
-- interruption after commit can lose the fetch; unfinished rows are deliberately not retried.
CREATE TABLE email_pixel_opens (
    email_id bigint PRIMARY KEY REFERENCES emails ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status text NOT NULL DEFAULT 'started' CHECK (status IN ('started', 'finished', 'busy')),
    pixels integer NOT NULL CHECK (pixels BETWEEN 1 AND 5),
    attempted integer NOT NULL DEFAULT 0 CHECK (attempted BETWEEN 0 AND pixels),
    opened integer NOT NULL DEFAULT 0 CHECK (opened BETWEEN 0 AND attempted)
);
ALTER TABLE email_pixel_opens ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON email_pixel_opens FOR SELECT USING (account_id = current_account_id());
