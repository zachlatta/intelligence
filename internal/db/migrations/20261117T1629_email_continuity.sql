-- Each follow-up keeps its own outcome while referring to the run whose agent it resumes.
ALTER TABLE workflow_runs ADD COLUMN continued_run_id uuid REFERENCES workflow_runs;
ALTER TABLE workflow_run_events ADD COLUMN continued_run_id uuid REFERENCES workflow_runs;
