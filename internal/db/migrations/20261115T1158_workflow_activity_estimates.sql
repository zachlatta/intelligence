ALTER TABLE workflow_runs
 ADD COLUMN activity_eta_estimated boolean NOT NULL DEFAULT false,
 ADD COLUMN activity_waiting_on text NOT NULL DEFAULT '',
 ADD CONSTRAINT activity_estimate_has_eta CHECK (NOT activity_eta_estimated OR activity_eta IS NOT NULL);
COMMENT ON COLUMN workflow_runs.activity_eta_estimated IS 'True for a server estimate from the last 30 days of completed workflow step times; false for agent ETAs or unknown.';
COMMENT ON COLUMN workflow_runs.activity_waiting_on IS 'Person or external party holding up the run; replaces its completion ETA.';
