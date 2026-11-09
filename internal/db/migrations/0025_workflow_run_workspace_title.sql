-- The title of a run's workspace (its workflow's name and the task's key words), set with its
-- branch when the workspace is queued; '' before then. Runs from before it keep ''.
ALTER TABLE workflow_runs ADD COLUMN workspace_title text NOT NULL DEFAULT '';
