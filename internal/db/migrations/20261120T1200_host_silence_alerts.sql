-- Hosts that stopped reporting while workflow runs depended on them (internal/hostwatch).
--
-- On 2026-11-19 a host ran out of memory and stopped answering; the server logged the lost stream
-- but told nobody, and runs queued for it and replies to its runs waited for two hours. Now, when a
-- host with active runs (queued, starting, running, waiting or stalled) has not been heard from
-- (hosts.last_seen) for 10 minutes, the server opens one alert row here and pushes one phone
-- notification through PDW, and one more when the host reports again. The open row is what the red
-- banner on every page shows (host_silence_current). One open row per host, so it never repeats
-- while the host stays down, on any server instance or after a restart.
CREATE TABLE host_silence_alerts (
 id bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
 host text NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
 last_seen timestamptz NOT NULL,             -- when the server last heard from the host before it went quiet
 opened_at timestamptz NOT NULL,             -- when the server noticed (10 minutes on)
 active_runs integer NOT NULL,               -- runs on the host when it was noticed
 running_runs integer NOT NULL DEFAULT 0,    -- finding_project, starting, running or stalled
 waiting_runs integer NOT NULL DEFAULT 0,    -- waiting for an answer
 queued_runs integer NOT NULL DEFAULT 0,
 oom_kills integer NOT NULL DEFAULT 0,       -- the host's last OOM kills: those its probe recorded in the hour up to its latest
 last_oom_at timestamptz,                    -- that latest kill
 oom_comms text NOT NULL DEFAULT '',         -- the processes those kills hit, comma separated
 notify boolean NOT NULL,                    -- whether a phone notification is sent for it (the host's account is the notified one)
 alert_text text NOT NULL DEFAULT '',        -- the notification, title and body
 alert_sent_at timestamptz,
 alert_attempts integer NOT NULL DEFAULT 0,
 alert_error text NOT NULL DEFAULT '',
 recovered_at timestamptz,                   -- when the host was heard from again
 recovery_text text NOT NULL DEFAULT '',
 recovery_sent_at timestamptz,
 recovery_attempts integer NOT NULL DEFAULT 0,
 recovery_error text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX host_silence_alerts_open ON host_silence_alerts (host) WHERE recovered_at IS NULL;
CREATE INDEX host_silence_alerts_opened ON host_silence_alerts (opened_at DESC);

ALTER TABLE host_silence_alerts ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON host_silence_alerts FOR SELECT
 USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));

-- What the banner shows: each host that is silent now, with how long and what waits on it.
CREATE VIEW host_silence_current WITH (security_invoker = true) AS
SELECT a.id, a.host, h.last_seen, now() - h.last_seen AS silent_for, a.opened_at,
       (SELECT count(*) FROM workflow_runs r WHERE r.host = a.host
          AND r.status IN ('queued','finding_project','starting','running','waiting','stalled'))::integer AS active_runs,
       a.oom_kills, a.last_oom_at, a.oom_comms, a.notify, a.alert_sent_at, a.alert_error
FROM host_silence_alerts a JOIN hosts h ON h.name = a.host
WHERE a.recovered_at IS NULL;
