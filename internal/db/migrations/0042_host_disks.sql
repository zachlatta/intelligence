-- How hard each host's disks work and how full each of its filesystems is (internal/resources).

-- Each minute, across the host's physical disks (partitions and the devices built on them, like
-- device mapper, RAID and loop devices, are counted once, in their disk): bytes read and written
-- a second and operations a second, the minute's average and its busiest 5 seconds; and the
-- busiest disk's share of the time it had I/O in flight (Linux's %util: near 100 the disk is
-- saturated), averaged and at its peak. Null is unknown: the host's first minute, an older host
-- binary, or a system that does not say (macOS has no busy time).
ALTER TABLE host_minutes
    ADD COLUMN disk_read_bps      real,
    ADD COLUMN disk_write_bps     real,
    ADD COLUMN disk_iops          real,
    ADD COLUMN disk_read_bps_max  real,
    ADD COLUMN disk_write_bps_max real,
    ADD COLUMN disk_busy_avg_pct  real,
    ADD COLUMN disk_busy_max_pct  real;

-- Each host's local filesystems as of its newest reading (once a minute): total and available
-- space (available to an ordinary user, less the root reserve). Network filesystems are left out.
-- Each local filesystem appears once, however many places it is mounted; a ZFS pool once.
CREATE TABLE host_filesystems (
    host      text NOT NULL,
    mount     text NOT NULL,
    device    text NOT NULL DEFAULT '',
    fs_type   text NOT NULL DEFAULT '',
    total     bigint NOT NULL,
    available bigint NOT NULL,
    used_frac double precision GENERATED ALWAYS AS
        (CASE WHEN total > 0 THEN (total - available)::double precision / total END) STORED,
    minute    timestamptz NOT NULL, -- the minute it was read in
    PRIMARY KEY (host, mount)
);
ALTER TABLE host_filesystems ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON host_filesystems FOR SELECT
    USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));

-- host_current gains the new columns (it selects h.*) and the load per CPU.
DROP VIEW host_current;
CREATE VIEW host_current WITH (security_invoker = true) AS
SELECT DISTINCT ON (host) h.*,
       CASE WHEN mem_total > 0 THEN 1 - mem_available_min::double precision / mem_total END AS mem_used_frac,
       CASE WHEN disk_total > 0 THEN 1 - disk_available_min::double precision / disk_total END AS disk_used_frac,
       minute > now() - interval '3 minutes' AS fresh,
       CASE WHEN cpus > 0 THEN load1::double precision / cpus END AS load1_per_cpu
FROM host_minutes h
ORDER BY host, minute DESC;

-- host_days gains the day's highest load, fullest home filesystem, busiest disk and bytes moved.
CREATE OR REPLACE VIEW host_days WITH (security_invoker = true) AS
SELECT host, date_trunc('day', minute) AS day,
       avg(cpu_avg_pct) AS cpu_avg_pct, max(cpu_max_pct) AS cpu_max_pct,
       max(1 - mem_available_min::double precision / nullif(mem_total, 0)) AS mem_used_frac_max,
       max(psi_mem_some) AS psi_mem_some_max, max(psi_mem_full) AS psi_mem_full_max,
       sum(oom_kills) AS oom_kills, max(paseo_mem_max) AS paseo_mem_max,
       max(load1) AS load1_max, max(load1::double precision / nullif(cpus, 0)) AS load1_per_cpu_max,
       max(1 - disk_available_min::double precision / nullif(disk_total, 0)) AS disk_used_frac_max,
       avg(disk_busy_avg_pct) AS disk_busy_avg_pct, max(disk_busy_max_pct) AS disk_busy_max_pct,
       sum(disk_read_bps::double precision * 60) AS disk_read_bytes, sum(disk_write_bps::double precision * 60) AS disk_write_bytes
FROM host_minutes
GROUP BY host, date_trunc('day', minute);
