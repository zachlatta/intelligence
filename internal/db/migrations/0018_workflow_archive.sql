-- Cleanup has its own durable command, so a failure never changes a run's outcome.
-- Join commands for its progress, error, result and expiry through read-only SQL.
ALTER TABLE workflow_runs ADD COLUMN archive_command uuid REFERENCES commands ON DELETE SET NULL;
