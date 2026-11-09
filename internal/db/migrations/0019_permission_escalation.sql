ALTER TABLE permission_resolutions DROP CONSTRAINT permission_resolutions_status_check;
ALTER TABLE permission_resolutions ADD CONSTRAINT permission_resolutions_status_check
 CHECK (status IN ('pending','denied','error','stuck','cleared'));
ALTER TABLE permission_resolutions
 ADD COLUMN first_seen_at timestamptz,
 ADD COLUMN denied_at timestamptz,
 ADD COLUMN stuck_at timestamptz,
 ADD COLUMN cleared_at timestamptz;
UPDATE permission_resolutions SET first_seen_at = attempted_at,
 denied_at = CASE WHEN status = 'denied' THEN attempted_at END;
ALTER TABLE permission_resolutions ALTER COLUMN first_seen_at SET NOT NULL;
ALTER TABLE permission_resolutions ALTER COLUMN first_seen_at SET DEFAULT now();
