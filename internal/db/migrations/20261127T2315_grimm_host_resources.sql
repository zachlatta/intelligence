-- Live host resource overview for a Grimm session, readable through /api/sql.
-- Combines the session's host's newest host_current reading, online status, eBPF probe status,
-- local filesystems, recent OOM kills (last 24h), and the session's busiest agents over the last
-- 15 minutes of readings. Always returns one row per session (has_reading = false before the host
-- has sent its first minute).

CREATE VIEW grimm_host_resources WITH (security_invoker = true) AS
SELECT s.id AS session_id,
       s.host,
       coalesce(hs.online, false) AS host_online,
       (c.minute IS NOT NULL) AS has_reading,
       c.minute,
       coalesce(c.fresh, false) AS fresh,
       coalesce(c.cpus, 0) AS cpus,
       c.cpu_avg_pct,
       c.cpu_max_pct,
       c.load1,
       c.load5,
       c.load15,
       c.load1_per_cpu,
       c.mem_total,
       c.mem_available_min,
       c.mem_used_frac,
       c.swap_total,
       c.swap_used_max,
       c.disk_total,
       c.disk_available_min,
       c.disk_used_frac,
       c.disk_read_bps,
       c.disk_write_bps,
       c.disk_iops,
       c.disk_read_bps_max,
       c.disk_write_bps_max,
       c.disk_busy_avg_pct,
       c.disk_busy_max_pct,
       c.psi_cpu_some,
       c.psi_mem_some,
       c.psi_mem_full,
       c.psi_io_some,
       c.psi_io_full,
       c.oom_kills,
       c.paseo_mem_max,
       c.paseo_mem_peak,
       c.paseo_oom_kills,
       p.running AS probe_running,
       coalesce(p.version, '') AS probe_version,
       coalesce(p.error, '') AS probe_error,
       coalesce(p.tracked, 0) AS probe_tracked,
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'mount', f.mount,
               'device', f.device,
               'fs_type', f.fs_type,
               'total', f.total,
               'available', f.available,
               'used_frac', f.used_frac
           ) ORDER BY f.used_frac DESC NULLS LAST, f.mount)
           FROM host_filesystems f
           WHERE f.host = s.host
       ), '[]'::jsonb) AS filesystems,
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'at', o.at,
               'pid', o.pid,
               'comm', o.comm,
               'agent_id', o.agent_id,
               'anon_rss', o.anon_rss
           ) ORDER BY o.at DESC)
           FROM (
               SELECT at, pid, comm, agent_id, anon_rss
               FROM oom_kills
               WHERE host = s.host AND at > now() - interval '24 hours'
               ORDER BY at DESC
               LIMIT 8
           ) o
       ), '[]'::jsonb) AS recent_ooms,
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'name', a.name,
               'role', a.role,
               'agent_id', a.agent_id,
               'mem_max', a.mem_max,
               'cpu_seconds', a.cpu_seconds,
               'processes_max', a.processes_max,
               'last_minute', a.last_minute
           ) ORDER BY a.mem_max DESC, a.cpu_seconds DESC, a.name)
           FROM (
               SELECT ga.name,
                      ga.role,
                      ga.agent_id,
                      max(am.mem_max) AS mem_max,
                      coalesce(sum(am.cpu_seconds), 0) AS cpu_seconds,
                      max(am.processes_max) AS processes_max,
                      max(am.minute) AS last_minute
               FROM grimm_agents ga
               JOIN agent_minutes am
                 ON am.host = ga.host
                AND am.agent_id = ga.agent_id
                AND am.minute >= coalesce(c.minute, now()) - interval '15 minutes'
               WHERE ga.session_id = s.id
               GROUP BY ga.name, ga.role, ga.agent_id
               ORDER BY max(am.mem_max) DESC, coalesce(sum(am.cpu_seconds), 0) DESC, ga.name
               LIMIT 8
           ) a
       ), '[]'::jsonb) AS session_agents
FROM grimm_sessions s
LEFT JOIN host_status hs ON hs.name = s.host
LEFT JOIN host_current c ON c.host = s.host
LEFT JOIN host_probes p ON p.host = s.host;
