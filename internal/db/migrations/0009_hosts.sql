-- Each dev box's resources over time (internal/host): the host every minute, each agent's
-- processes every minute, and every process the kernel killed for memory. CPU per agent comes
-- from the box's eBPF probe (internal/probe) where it runs: every process, exact; otherwise from
-- /proc, which sees only processes alive at a walk (every 15 seconds).

-- One row per box per minute: averages and extremes of its 5-second samples. Null is unknown
-- (macOS has no pressure or cgroups).
CREATE TABLE host_minutes (
    box                text NOT NULL,
    minute             timestamptz NOT NULL,
    samples            integer NOT NULL,
    cpus               integer NOT NULL DEFAULT 0,
    cpu_avg_pct        real,
    cpu_max_pct        real,
    load1              real NOT NULL DEFAULT 0,
    load5              real NOT NULL DEFAULT 0,
    load15             real NOT NULL DEFAULT 0,
    mem_total          bigint NOT NULL DEFAULT 0,
    mem_available_min  bigint NOT NULL DEFAULT 0,
    swap_total         bigint NOT NULL DEFAULT 0,
    swap_used_max      bigint NOT NULL DEFAULT 0,
    disk_total         bigint NOT NULL DEFAULT 0,  -- the home filesystem
    disk_available_min bigint NOT NULL DEFAULT 0,
    psi_cpu_some       real,                       -- pressure: the most of the minute's 10-second averages, %
    psi_mem_some       real,
    psi_mem_full       real,
    psi_io_some        real,
    psi_io_full        real,
    oom_kills          integer,                    -- the kernel's OOM kills during the minute
    paseo_mem_max      bigint,                     -- the Paseo daemon's cgroup (all its agents)
    paseo_mem_peak     bigint,
    paseo_oom_kills    integer,
    PRIMARY KEY (box, minute)
);

-- One row per agent per minute it had processes or used CPU.
CREATE TABLE agent_minutes (
    box           text NOT NULL,
    agent_id      text NOT NULL,
    minute        timestamptz NOT NULL,
    cpu_seconds   real,                -- null unknown (the probe's first minute)
    cpu_source    text NOT NULL DEFAULT '' CHECK (cpu_source IN ('', 'ebpf', 'proc')),
    mem_max       bigint NOT NULL DEFAULT 0, -- its processes' proportional set size, the most seen
    processes_max integer NOT NULL DEFAULT 0,
    execs         integer,             -- programs started (the probe only)
    PRIMARY KEY (box, agent_id, minute)
);
CREATE INDEX agent_minutes_minute ON agent_minutes (minute);

-- Every process the probe saw the kernel kill for memory.
CREATE TABLE oom_kills (
    box       text NOT NULL,
    at        timestamptz NOT NULL,
    pid       integer NOT NULL,
    comm      text NOT NULL DEFAULT '',
    agent_id  text NOT NULL DEFAULT '', -- '' none
    anon_rss  bigint NOT NULL DEFAULT 0,
    file_rss  bigint NOT NULL DEFAULT 0,
    shmem_rss bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (box, at, pid)
);

-- Each box's eBPF probe, as of its last report.
CREATE TABLE host_probes (
    box         text PRIMARY KEY,
    running     boolean NOT NULL,
    version     text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    tracked     integer NOT NULL DEFAULT 0,
    received_at timestamptz NOT NULL
);

-- Each box's newest minute, with what is used as fractions.
CREATE VIEW host_current AS
SELECT DISTINCT ON (box) h.*,
       CASE WHEN mem_total > 0 THEN 1 - mem_available_min::double precision / mem_total END AS mem_used_frac,
       CASE WHEN disk_total > 0 THEN 1 - disk_available_min::double precision / disk_total END AS disk_used_frac,
       minute > now() - interval '3 minutes' AS fresh
FROM host_minutes h
ORDER BY box, minute DESC;

-- Per box per day: the busiest and fullest it got, and the kills.
CREATE VIEW host_days AS
SELECT box, date_trunc('day', minute) AS day,
       avg(cpu_avg_pct) AS cpu_avg_pct, max(cpu_max_pct) AS cpu_max_pct,
       max(1 - mem_available_min::double precision / nullif(mem_total, 0)) AS mem_used_frac_max,
       max(psi_mem_some) AS psi_mem_some_max, max(psi_mem_full) AS psi_mem_full_max,
       sum(oom_kills) AS oom_kills, max(paseo_mem_max) AS paseo_mem_max
FROM host_minutes
GROUP BY box, date_trunc('day', minute);

-- Per agent per day: CPU used, the most memory, programs started.
CREATE VIEW agent_days AS
SELECT box, agent_id, date_trunc('day', minute) AS day,
       sum(cpu_seconds) AS cpu_seconds, max(mem_max) AS mem_max, sum(execs) AS execs,
       count(*) AS minutes
FROM agent_minutes
GROUP BY box, agent_id, date_trunc('day', minute);
