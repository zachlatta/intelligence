-- Canonical activity ETA for clients: stored agent ETAs survive a human wait,
-- but neither waits nor expired server estimates display a completion time.
CREATE VIEW workflow_run_activity WITH (security_invoker = true) AS
SELECT id AS run_id, account_id, activity_waiting_on,
    CASE WHEN activity_waiting_on <> '' OR
        (activity_eta_estimated AND activity_eta <= now())
        THEN NULL ELSE activity_eta END AS activity_eta,
    activity_eta_estimated AND activity_waiting_on = '' AND activity_eta > now()
        AS activity_eta_estimated
FROM workflow_runs;
COMMENT ON VIEW workflow_run_activity IS 'Display-safe activity ETA: hides human/external waits and expired server estimates; agent ETAs are preserved in workflow_runs.';
