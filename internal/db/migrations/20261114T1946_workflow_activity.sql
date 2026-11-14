ALTER TABLE workflow_runs
    ADD COLUMN current_activity text NOT NULL DEFAULT '' CHECK (char_length(current_activity) <= 280),
    ADD COLUMN activity_updated_at timestamptz,
    ADD COLUMN activity_eta timestamptz;
COMMENT ON COLUMN workflow_runs.current_activity IS 'Latest plain sentence from the orchestrator about what the run is doing now.';
COMMENT ON COLUMN workflow_runs.activity_updated_at IS 'When the orchestrator last reported current activity; independent of lifecycle updates.';
COMMENT ON COLUMN workflow_runs.activity_eta IS 'Estimated completion time, or NULL when unknown. Replaced on every status move.';
