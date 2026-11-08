package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/host"
)

// RecordHost stores a box's host report, received at at. Minutes already stored (a retried send)
// are replaced.
func (r *Registry) RecordHost(ctx context.Context, box string, rep host.Report, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, m := range rep.Minutes {
			b.Queue(`INSERT INTO host_minutes (box, minute, samples, cpus, cpu_avg_pct, cpu_max_pct, load1, load5, load15,
				mem_total, mem_available_min, swap_total, swap_used_max, disk_total, disk_available_min,
				psi_cpu_some, psi_mem_some, psi_mem_full, psi_io_some, psi_io_full, oom_kills, paseo_mem_max, paseo_mem_peak, paseo_oom_kills)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
				ON CONFLICT (box, minute) DO UPDATE SET samples = EXCLUDED.samples, cpus = EXCLUDED.cpus,
				cpu_avg_pct = EXCLUDED.cpu_avg_pct, cpu_max_pct = EXCLUDED.cpu_max_pct, load1 = EXCLUDED.load1,
				load5 = EXCLUDED.load5, load15 = EXCLUDED.load15, mem_total = EXCLUDED.mem_total,
				mem_available_min = EXCLUDED.mem_available_min, swap_total = EXCLUDED.swap_total,
				swap_used_max = EXCLUDED.swap_used_max, disk_total = EXCLUDED.disk_total,
				disk_available_min = EXCLUDED.disk_available_min, psi_cpu_some = EXCLUDED.psi_cpu_some,
				psi_mem_some = EXCLUDED.psi_mem_some, psi_mem_full = EXCLUDED.psi_mem_full, psi_io_some = EXCLUDED.psi_io_some,
				psi_io_full = EXCLUDED.psi_io_full, oom_kills = EXCLUDED.oom_kills, paseo_mem_max = EXCLUDED.paseo_mem_max,
				paseo_mem_peak = EXCLUDED.paseo_mem_peak, paseo_oom_kills = EXCLUDED.paseo_oom_kills`,
				box, m.Minute, m.Samples, m.CPUs, m.CPUAvgPct, m.CPUMaxPct, m.Load1, m.Load5, m.Load15,
				int64(m.MemTotal), int64(m.MemAvailableMin), int64(m.SwapTotal), int64(m.SwapUsedMax), int64(m.DiskTotal),
				int64(m.DiskAvailableMin), m.PSICPUSome, m.PSIMemSome, m.PSIMemFull, m.PSIIOSome, m.PSIIOFull,
				i64(m.OOMKills), i64(m.PaseoMemMax), i64(m.PaseoMemPeak), i64(m.PaseoOOMKills))
		}
		for _, a := range rep.Agents {
			if a.AgentID == "" {
				continue
			}
			b.Queue(`INSERT INTO agent_minutes (box, agent_id, minute, cpu_seconds, cpu_source, mem_max, processes_max, execs)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (box, agent_id, minute) DO UPDATE SET cpu_seconds = EXCLUDED.cpu_seconds,
				cpu_source = EXCLUDED.cpu_source, mem_max = EXCLUDED.mem_max, processes_max = EXCLUDED.processes_max,
				execs = EXCLUDED.execs`,
				box, a.AgentID, a.Minute, a.CPUSeconds, a.CPUSource, int64(a.MemMax), a.ProcessesMax, i64(a.Execs))
		}
		for _, o := range rep.OOMs {
			b.Queue(`INSERT INTO oom_kills (box, at, pid, comm, agent_id, anon_rss, file_rss, shmem_rss)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				box, o.At, o.PID, o.Comm, o.AgentID, int64(o.AnonRSS), int64(o.FileRSS), int64(o.ShmemRSS))
		}
		if p := rep.Probe; p != nil {
			b.Queue(`INSERT INTO host_probes (box, running, version, error, tracked, received_at) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (box) DO UPDATE SET running = EXCLUDED.running, version = EXCLUDED.version, error = EXCLUDED.error,
				tracked = EXCLUDED.tracked, received_at = EXCLUDED.received_at`,
				box, p.Running, p.Version, p.Error, p.Tracked, at)
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
}

const hostMinuteCols = `minute, cpus, cpu_avg_pct, cpu_max_pct, load1, mem_total, mem_available_min, swap_total, swap_used_max,
	disk_total, disk_available_min, psi_mem_some, psi_mem_full, psi_cpu_some, psi_io_some, oom_kills, paseo_mem_max`

func scanHostMinute(row pgx.Row) (HostMinute, error) {
	var m HostMinute
	var cpuAvg, cpuMax, memSome, memFull, cpuSome, ioSome *float32
	var load float32
	err := row.Scan(&m.Minute, &m.CPUs, &cpuAvg, &cpuMax, &load, &m.MemTotal, &m.MemAvailableMin, &m.SwapTotal, &m.SwapUsedMax,
		&m.DiskTotal, &m.DiskAvailableMin, &memSome, &memFull, &cpuSome, &ioSome, &m.OOMKills, &m.PaseoMemMax)
	m.Load1 = float64(load)
	m.CPUAvgPct, m.CPUMaxPct, m.PSIMemSome, m.PSIMemFull, m.PSICPUSome, m.PSIIOSome =
		f64(cpuAvg), f64(cpuMax), f64(memSome), f64(memFull), f64(cpuSome), f64(ioSome)
	m.Minute = m.Minute.UTC()
	return m, err
}

func f64(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

// HostNow is a box's newest minute, its probe, and each agent's memory now.
type HostNow struct {
	Minute       *HostMinute
	MemUsedFrac  float64
	DiskUsedFrac float64
	Fresh        bool // a minute within the last 3
	Probe        *ProbeRow
	AgentMem     map[string]int64 // agents with a minute in the last 3, their most memory
}

// ProbeRow is a box's probe, as last reported.
type ProbeRow struct {
	Running    bool      `json:"running"`
	Version    string    `json:"version"`
	Error      string    `json:"error"`
	Tracked    int       `json:"tracked"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// Hosts is every box's HostNow.
func (r *Registry) Hosts(ctx context.Context) (map[string]*HostNow, error) {
	out := map[string]*HostNow{}
	get := func(box string) *HostNow {
		if h := out[box]; h != nil {
			return h
		}
		h := &HostNow{AgentMem: map[string]int64{}}
		out[box] = h
		return h
	}
	rows, err := r.p.Query(ctx, `SELECT box, `+hostMinuteCols+`, coalesce(mem_used_frac, 0), coalesce(disk_used_frac, 0), fresh FROM host_current`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var box string
		var m HostMinute
		var cpuAvg, cpuMax, memSome, memFull, cpuSome, ioSome *float32
		var load float32
		var memFrac, diskFrac float64
		var fresh bool
		if err := rows.Scan(&box, &m.Minute, &m.CPUs, &cpuAvg, &cpuMax, &load, &m.MemTotal, &m.MemAvailableMin, &m.SwapTotal,
			&m.SwapUsedMax, &m.DiskTotal, &m.DiskAvailableMin, &memSome, &memFull, &cpuSome, &ioSome, &m.OOMKills, &m.PaseoMemMax,
			&memFrac, &diskFrac, &fresh); err != nil {
			rows.Close()
			return nil, err
		}
		m.Load1 = float64(load)
		m.CPUAvgPct, m.CPUMaxPct, m.PSIMemSome, m.PSIMemFull, m.PSICPUSome, m.PSIIOSome =
			f64(cpuAvg), f64(cpuMax), f64(memSome), f64(memFull), f64(cpuSome), f64(ioSome)
		h := get(box)
		h.Minute, h.MemUsedFrac, h.DiskUsedFrac, h.Fresh = &m, memFrac, diskFrac, fresh
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.p.Query(ctx, "SELECT box, running, version, error, tracked, received_at FROM host_probes")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var box string
		var p ProbeRow
		if err := rows.Scan(&box, &p.Running, &p.Version, &p.Error, &p.Tracked, &p.ReceivedAt); err != nil {
			rows.Close()
			return nil, err
		}
		get(box).Probe = &p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.p.Query(ctx, `SELECT DISTINCT ON (box, agent_id) box, agent_id, mem_max FROM agent_minutes
		WHERE minute > now() - interval '3 minutes' ORDER BY box, agent_id, minute DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var box, id string
		var mem int64
		if err := rows.Scan(&box, &id, &mem); err != nil {
			return nil, err
		}
		get(box).AgentMem[id] = mem
	}
	return out, rows.Err()
}

// HostHistory is a box's minutes since since, oldest first.
func (r *Registry) HostHistory(ctx context.Context, box string, since time.Time) ([]HostMinute, error) {
	rows, err := r.p.Query(ctx, `SELECT `+hostMinuteCols+` FROM host_minutes WHERE box = $1 AND minute >= $2 ORDER BY minute`, box, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HostMinute, error) { return scanHostMinute(row) })
}

// AgentTotal is one agent's use of a box over a period.
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

// TopAgents is every agent that used box since since, most CPU first.
func (r *Registry) TopAgents(ctx context.Context, box string, since time.Time) ([]AgentTotal, error) {
	rows, err := r.p.Query(ctx, `SELECT m.agent_id, coalesce(max(a.title), ''), coalesce(sum(m.cpu_seconds), 0),
		coalesce(max(m.cpu_source), ''), max(m.mem_max), coalesce(sum(m.execs), 0), count(*), max(m.minute)
		FROM agent_minutes m LEFT JOIN agents a ON a.box = m.box AND a.id = m.agent_id
		WHERE m.box = $1 AND m.minute >= $2
		GROUP BY m.agent_id ORDER BY 3 DESC, 5 DESC, 1`, box, since)
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
	Box     string    `json:"box"`
	At      time.Time `json:"at"`
	PID     int       `json:"pid"`
	Comm    string    `json:"comm"`
	AgentID string    `json:"agentId"`
	Title   string    `json:"title"`
	AnonRSS int64     `json:"anonRss"`
}

// OOMKills is every kill since since, newest first.
func (r *Registry) OOMKills(ctx context.Context, since time.Time) ([]OOMKill, error) {
	rows, err := r.p.Query(ctx, `SELECT k.box, k.at, k.pid, k.comm, k.agent_id, coalesce(a.title, ''), k.anon_rss
		FROM oom_kills k LEFT JOIN agents a ON a.box = k.box AND a.id = k.agent_id
		WHERE k.at >= $1 ORDER BY k.at DESC LIMIT 200`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OOMKill, error) {
		var k OOMKill
		err := row.Scan(&k.Box, &k.At, &k.PID, &k.Comm, &k.AgentID, &k.Title, &k.AnonRSS)
		return k, err
	})
}
