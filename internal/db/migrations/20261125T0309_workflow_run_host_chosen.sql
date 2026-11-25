-- A run started on a host other than its workflow's (picked in the task composer): its retries stay there.
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS host_chosen boolean NOT NULL DEFAULT false;
