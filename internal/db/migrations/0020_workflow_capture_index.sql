-- Finished runs no longer participate in report-time history capture.
CREATE INDEX workflow_runs_capturing ON workflow_runs (host, id) WHERE finished_at IS NULL;
