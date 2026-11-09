ALTER TABLE permission_resolutions
 ADD COLUMN next_retry_at timestamptz,
 ADD COLUMN retry_delay_ns bigint NOT NULL DEFAULT 0 CHECK (retry_delay_ns >= 0);
