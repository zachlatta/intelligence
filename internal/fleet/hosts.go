package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/resources"
)

// RecordHost stores a host's host report, received at at. Minutes already stored (a retried send)
// are replaced.
func (r *Registry) RecordHost(ctx context.Context, host string, rep resources.Report, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		var newest *resources.Minute
		for i, m := range rep.Minutes {
			b.Queue(`INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, cpu_max_pct, load1, load5, load15,
				mem_total, mem_available_min, swap_total, swap_used_max, disk_total, disk_available_min,
				psi_cpu_some, psi_mem_some, psi_mem_full, psi_io_some, psi_io_full, oom_kills, paseo_mem_max, paseo_mem_peak, paseo_oom_kills,
				disk_read_bps, disk_write_bps, disk_iops, disk_read_bps_max, disk_write_bps_max, disk_busy_avg_pct, disk_busy_max_pct)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
				$25, $26, $27, $28, $29, $30, $31)
				ON CONFLICT (host, minute) DO UPDATE SET samples = EXCLUDED.samples, cpus = EXCLUDED.cpus,
				cpu_avg_pct = EXCLUDED.cpu_avg_pct, cpu_max_pct = EXCLUDED.cpu_max_pct, load1 = EXCLUDED.load1,
				load5 = EXCLUDED.load5, load15 = EXCLUDED.load15, mem_total = EXCLUDED.mem_total,
				mem_available_min = EXCLUDED.mem_available_min, swap_total = EXCLUDED.swap_total,
				swap_used_max = EXCLUDED.swap_used_max, disk_total = EXCLUDED.disk_total,
				disk_available_min = EXCLUDED.disk_available_min, psi_cpu_some = EXCLUDED.psi_cpu_some,
				psi_mem_some = EXCLUDED.psi_mem_some, psi_mem_full = EXCLUDED.psi_mem_full, psi_io_some = EXCLUDED.psi_io_some,
				psi_io_full = EXCLUDED.psi_io_full, oom_kills = EXCLUDED.oom_kills, paseo_mem_max = EXCLUDED.paseo_mem_max,
				paseo_mem_peak = EXCLUDED.paseo_mem_peak, paseo_oom_kills = EXCLUDED.paseo_oom_kills,
				disk_read_bps = EXCLUDED.disk_read_bps, disk_write_bps = EXCLUDED.disk_write_bps, disk_iops = EXCLUDED.disk_iops,
				disk_read_bps_max = EXCLUDED.disk_read_bps_max, disk_write_bps_max = EXCLUDED.disk_write_bps_max,
				disk_busy_avg_pct = EXCLUDED.disk_busy_avg_pct, disk_busy_max_pct = EXCLUDED.disk_busy_max_pct`,
				host, m.Minute, m.Samples, m.CPUs, m.CPUAvgPct, m.CPUMaxPct, m.Load1, m.Load5, m.Load15,
				int64(m.MemTotal), int64(m.MemAvailableMin), int64(m.SwapTotal), int64(m.SwapUsedMax), int64(m.DiskTotal),
				int64(m.DiskAvailableMin), m.PSICPUSome, m.PSIMemSome, m.PSIMemFull, m.PSIIOSome, m.PSIIOFull,
				i64(m.OOMKills), i64(m.PaseoMemMax), i64(m.PaseoMemPeak), i64(m.PaseoOOMKills),
				m.DiskReadBps, m.DiskWriteBps, m.DiskIOPS, m.DiskReadBpsMax, m.DiskWriteBpsMax, m.DiskBusyAvgPct, m.DiskBusyMaxPct)
			if m.Filesystems != nil && (newest == nil || m.Minute.After(newest.Minute)) {
				newest = &rep.Minutes[i]
			}
		}
		if newest != nil {
			// the newest reading replaces the host's filesystems, unless a newer one is stored
			b.Queue(`DELETE FROM host_filesystems WHERE host = $1 AND minute <= $2`, host, newest.Minute)
			for _, f := range newest.Filesystems {
				b.Queue(`INSERT INTO host_filesystems (host, mount, device, fs_type, total, available, minute)
					SELECT $1, $2, $3, $4, $5, $6, $7
					WHERE NOT EXISTS (SELECT 1 FROM host_filesystems WHERE host = $1 AND minute > $7)
					ON CONFLICT (host, mount) DO NOTHING`,
					host, f.Mount, f.Device, f.Type, int64(f.Total), int64(f.Available), newest.Minute)
			}
		}
		for _, a := range rep.Agents {
			if a.AgentID == "" {
				continue
			}
			b.Queue(`INSERT INTO agent_minutes (host, agent_id, minute, cpu_seconds, cpu_source, mem_max, processes_max, execs)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (host, agent_id, minute) DO UPDATE SET cpu_seconds = EXCLUDED.cpu_seconds,
				cpu_source = EXCLUDED.cpu_source, mem_max = EXCLUDED.mem_max, processes_max = EXCLUDED.processes_max,
				execs = EXCLUDED.execs`,
				host, a.AgentID, a.Minute, a.CPUSeconds, a.CPUSource, int64(a.MemMax), a.ProcessesMax, i64(a.Execs))
		}
		for _, o := range rep.OOMs {
			b.Queue(`INSERT INTO oom_kills (host, at, pid, comm, agent_id, anon_rss, file_rss, shmem_rss)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				host, o.At, o.PID, o.Comm, o.AgentID, int64(o.AnonRSS), int64(o.FileRSS), int64(o.ShmemRSS))
		}
		if p := rep.Probe; p != nil {
			b.Queue(`INSERT INTO host_probes (host, running, version, error, tracked, received_at) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (host) DO UPDATE SET running = EXCLUDED.running, version = EXCLUDED.version, error = EXCLUDED.error,
				tracked = EXCLUDED.tracked, received_at = EXCLUDED.received_at`,
				host, p.Running, p.Version, p.Error, p.Tracked, at)
		}
		return tx.SendBatch(ctx, b).Close()
	})
}

func i64(p *uint64) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

// PruneHosts deletes host and agent minutes before before. Kills are kept.
func (r *Registry) PruneHosts(ctx context.Context, before time.Time) error {
	_, err := r.p.Exec(ctx, `DELETE FROM host_minutes WHERE minute < $1`, before)
	if err != nil {
		return err
	}
	_, err = r.p.Exec(ctx, `DELETE FROM agent_minutes WHERE minute < $1`, before)
	return err
}

// HostMinute is a stored minute.
type HostMinute struct {
	Minute           time.Time `json:"minute"`
	CPUs             int       `json:"cpus"`
	CPUAvgPct        *float64  `json:"cpuAvgPct"`
	CPUMaxPct        *float64  `json:"cpuMaxPct"`
	Load1            float64   `json:"load1"`
	Load5            float64   `json:"load5"`
	Load15           float64   `json:"load15"`
	MemTotal         int64     `json:"memTotal"`
	MemAvailableMin  int64     `json:"memAvailableMin"`
	SwapTotal        int64     `json:"swapTotal"`
	SwapUsedMax      int64     `json:"swapUsedMax"`
	DiskTotal        int64     `json:"diskTotal"`
	DiskAvailableMin int64     `json:"diskAvailableMin"`
	PSIMemSome       *float64  `json:"psiMemSome"`
	PSIMemFull       *float64  `json:"psiMemFull"`
	PSICPUSome       *float64  `json:"psiCpuSome"`
	PSIIOSome        *float64  `json:"psiIoSome"`
	OOMKills         *int64    `json:"oomKills"`
	PaseoMemMax      *int64    `json:"paseoMemMax"`
	DiskReadBps      *float64  `json:"diskReadBps"`
	DiskWriteBps     *float64  `json:"diskWriteBps"`
	DiskIOPS         *float64  `json:"diskIops"`
	DiskReadBpsMax   *float64  `json:"diskReadBpsMax"`
	DiskWriteBpsMax  *float64  `json:"diskWriteBpsMax"`
	DiskBusyAvgPct   *float64  `json:"diskBusyAvgPct"`
	DiskBusyMaxPct   *float64  `json:"diskBusyMaxPct"`
}

const hostMinuteCols = `minute, cpus, cpu_avg_pct, cpu_max_pct, load1, load5, load15, mem_total, mem_available_min, swap_total,
	swap_used_max, disk_total, disk_available_min, psi_mem_some, psi_mem_full, psi_cpu_some, psi_io_some, oom_kills, paseo_mem_max,
	disk_read_bps, disk_write_bps, disk_iops, disk_read_bps_max, disk_write_bps_max, disk_busy_avg_pct, disk_busy_max_pct`

// hostMinuteScan is where row.Scan puts hostMinuteCols, and done, which finishes m once scanned.
func hostMinuteScan(m *HostMinute) (dest []any, done func()) {
	var load1, load5, load15 float32
	var reals [13]*float32
	dest = []any{&m.Minute, &m.CPUs, &reals[0], &reals[1], &load1, &load5, &load15, &m.MemTotal, &m.MemAvailableMin, &m.SwapTotal,
		&m.SwapUsedMax, &m.DiskTotal, &m.DiskAvailableMin, &reals[2], &reals[3], &reals[4], &reals[5], &m.OOMKills, &m.PaseoMemMax,
		&reals[6], &reals[7], &reals[8], &reals[9], &reals[10], &reals[11], &reals[12]}
	return dest, func() {
		m.Load1, m.Load5, m.Load15 = float64(load1), float64(load5), float64(load15)
		for i, p := range []**float64{&m.CPUAvgPct, &m.CPUMaxPct, &m.PSIMemSome, &m.PSIMemFull, &m.PSICPUSome, &m.PSIIOSome,
			&m.DiskReadBps, &m.DiskWriteBps, &m.DiskIOPS, &m.DiskReadBpsMax, &m.DiskWriteBpsMax, &m.DiskBusyAvgPct, &m.DiskBusyMaxPct} {
			*p = f64(reals[i])
		}
		m.Minute = m.Minute.UTC()
	}
}

func scanHostMinute(row pgx.Row) (HostMinute, error) {
	var m HostMinute
	dest, done := hostMinuteScan(&m)
	err := row.Scan(dest...)
	done()
	return m, err
}

func f64(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

// HostNow is a host's newest minute, its probe, and each agent's memory now.
type HostNow struct {
	Minute       *HostMinute
	MemUsedFrac  float64
	DiskUsedFrac float64
	Fresh        bool // a minute within the last 3
	Probe        *ProbeRow
	AgentMem     map[string]int64 // agents with a minute in the last 3, their most memory
	Filesystems  []FilesystemRow  // its local filesystems, as last read, by mount
}

// FilesystemRow is one of a host's filesystems, as last read.
type FilesystemRow struct {
	Mount     string    `json:"mount"`
	Device    string    `json:"device"`
	Type      string    `json:"type"`
	Total     int64     `json:"total"`
	Available int64     `json:"available"`
	UsedFrac  float64   `json:"usedFrac"`
	Minute    time.Time `json:"minute"`
}

// ProbeRow is a host's probe, as last reported.
type ProbeRow struct {
	Running    bool      `json:"running"`
	Version    string    `json:"version"`
	Error      string    `json:"error"`
	Tracked    int       `json:"tracked"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// HostResources is every host's HostNow.
func (r *Registry) HostResources(ctx context.Context) (map[string]*HostNow, error) {
	out := map[string]*HostNow{}
	get := func(host string) *HostNow {
		if h := out[host]; h != nil {
			return h
		}
		h := &HostNow{AgentMem: map[string]int64{}}
		out[host] = h
		return h
	}
	rows, err := r.p.Query(ctx, `SELECT host, `+hostMinuteCols+`, coalesce(mem_used_frac, 0), coalesce(disk_used_frac, 0), fresh FROM host_current`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host string
		var m HostMinute
		var memFrac, diskFrac float64
		var fresh bool
		dest, done := hostMinuteScan(&m)
		if err := rows.Scan(append(append([]any{&host}, dest...), &memFrac, &diskFrac, &fresh)...); err != nil {
			rows.Close()
			return nil, err
		}
		done()
		h := get(host)
		h.Minute, h.MemUsedFrac, h.DiskUsedFrac, h.Fresh = &m, memFrac, diskFrac, fresh
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.p.Query(ctx, `SELECT host, mount, device, fs_type, total, available, coalesce(used_frac, 0), minute
		FROM host_filesystems ORDER BY host, mount`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host string
		var f FilesystemRow
		if err := rows.Scan(&host, &f.Mount, &f.Device, &f.Type, &f.Total, &f.Available, &f.UsedFrac, &f.Minute); err != nil {
			rows.Close()
			return nil, err
		}
		f.Minute = f.Minute.UTC()
		h := get(host)
		h.Filesystems = append(h.Filesystems, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.p.Query(ctx, "SELECT host, running, version, error, tracked, received_at FROM host_probes")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host string
		var p ProbeRow
		if err := rows.Scan(&host, &p.Running, &p.Version, &p.Error, &p.Tracked, &p.ReceivedAt); err != nil {
			rows.Close()
			return nil, err
		}
		get(host).Probe = &p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.p.Query(ctx, `SELECT DISTINCT ON (host, agent_id) host, agent_id, mem_max FROM agent_minutes
		WHERE minute > now() - interval '3 minutes' ORDER BY host, agent_id, minute DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var host, id string
		var mem int64
		if err := rows.Scan(&host, &id, &mem); err != nil {
			return nil, err
		}
		get(host).AgentMem[id] = mem
	}
	return out, rows.Err()
}

// HostHistory is a host's minutes since since, oldest first.
func (r *Registry) HostHistory(ctx context.Context, host string, since time.Time) ([]HostMinute, error) {
	rows, err := r.p.Query(ctx, `SELECT `+hostMinuteCols+` FROM host_minutes WHERE host = $1 AND minute >= $2 ORDER BY minute`, host, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HostMinute, error) { return scanHostMinute(row) })
}

// AgentTotal is one agent's use of a host over a period.
type AgentTotal struct {
	AgentID    string    `json:"agentId"`
	Title      string    `json:"title"` // from agents, while Paseo lists it
	CPUSeconds float64   `json:"cpuSeconds"`
	CPUSource  string    `json:"cpuSource"`
	MemMax     int64     `json:"memMax"`
	Execs      int64     `json:"execs"`
	Minutes    int       `json:"minutes"`
	LastSeen   time.Time `json:"lastSeen"`
}

// TopAgents is every agent that used host since since, most CPU first.
func (r *Registry) TopAgents(ctx context.Context, host string, since time.Time) ([]AgentTotal, error) {
	rows, err := r.p.Query(ctx, `SELECT m.agent_id, coalesce(max(a.title), ''), coalesce(sum(m.cpu_seconds), 0),
		CASE WHEN bool_or(m.cpu_source = 'ebpf') THEN 'ebpf' ELSE coalesce(max(m.cpu_source), '') END, max(m.mem_max), coalesce(sum(m.execs), 0), count(*), max(m.minute)
		FROM agent_minutes m LEFT JOIN agents a ON a.host = m.host AND a.id = m.agent_id
		WHERE m.host = $1 AND m.minute >= $2
		GROUP BY m.agent_id ORDER BY 3 DESC, 5 DESC, 1`, host, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentTotal, error) {
		var a AgentTotal
		var cpu float64
		err := row.Scan(&a.AgentID, &a.Title, &cpu, &a.CPUSource, &a.MemMax, &a.Execs, &a.Minutes, &a.LastSeen)
		a.CPUSeconds = cpu
		return a, err
	})
}

// OOMKill is a stored kill.
type OOMKill struct {
	Host    string    `json:"host"`
	At      time.Time `json:"at"`
	PID     int       `json:"pid"`
	Comm    string    `json:"comm"`
	AgentID string    `json:"agentId"`
	Title   string    `json:"title"`
	AnonRSS int64     `json:"anonRss"`
}

// OOMKills is every kill since since, newest first.
func (r *Registry) OOMKills(ctx context.Context, since time.Time) ([]OOMKill, error) {
	rows, err := r.p.Query(ctx, `SELECT k.host, k.at, k.pid, k.comm, k.agent_id, coalesce(a.title, ''), k.anon_rss
		FROM oom_kills k LEFT JOIN agents a ON a.host = k.host AND a.id = k.agent_id
		WHERE k.at >= $1 ORDER BY k.at DESC LIMIT 200`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OOMKill, error) {
		var k OOMKill
		err := row.Scan(&k.Host, &k.At, &k.PID, &k.Comm, &k.AgentID, &k.Title, &k.AnonRSS)
		return k, err
	})
}

// AgentMinute is one of an agent's stored minutes.
type AgentMinute struct {
	Minute       time.Time `json:"minute"`
	CPUSeconds   *float64  `json:"cpuSeconds"`
	CPUSource    string    `json:"cpuSource"`
	MemMax       int64     `json:"memMax"`
	ProcessesMax int       `json:"processesMax"`
	Execs        *int64    `json:"execs"`
}

// AgentUse is everything recorded of one agent's resources: its minutes (oldest first), the
// processes the kernel killed for memory, and how many CPUs its host has.
type AgentUse struct {
	Minutes []AgentMinute
	OOMs    []OOMKill
	CPUs    int
}

// AgentUse reads an agent's minutes and kills on host.
func (r *Registry) AgentUse(ctx context.Context, host, agentID string) (AgentUse, error) {
	var u AgentUse
	rows, err := r.p.Query(ctx, `SELECT minute, cpu_seconds, cpu_source, mem_max, processes_max, execs
		FROM agent_minutes WHERE host = $1 AND agent_id = $2 ORDER BY minute`, host, agentID)
	if err != nil {
		return u, err
	}
	u.Minutes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentMinute, error) {
		var m AgentMinute
		var cpu *float32
		err := row.Scan(&m.Minute, &cpu, &m.CPUSource, &m.MemMax, &m.ProcessesMax, &m.Execs)
		m.CPUSeconds, m.Minute = f64(cpu), m.Minute.UTC()
		return m, err
	})
	if err != nil {
		return u, err
	}
	rows, err = r.p.Query(ctx, `SELECT host, at, pid, comm, agent_id, '', anon_rss FROM oom_kills
		WHERE host = $1 AND agent_id = $2 ORDER BY at DESC LIMIT 200`, host, agentID)
	if err != nil {
		return u, err
	}
	u.OOMs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (OOMKill, error) {
		var k OOMKill
		err := row.Scan(&k.Host, &k.At, &k.PID, &k.Comm, &k.AgentID, &k.Title, &k.AnonRSS)
		return k, err
	})
	if err != nil {
		return u, err
	}
	err = r.p.QueryRow(ctx, `SELECT coalesce((SELECT cpus FROM host_current WHERE host = $1), 0)`, host).Scan(&u.CPUs)
	return u, err
}
