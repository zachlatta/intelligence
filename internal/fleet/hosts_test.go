package fleet

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/probe"
	"github.com/zachlatta/intelligence/internal/resources"
)

func pf(v float64) *float64 { return &v }
func pu(v uint64) *uint64   { return &v }

func hostReport(m time.Time) resources.Report {
	return resources.Report{
		Minutes: []resources.Minute{{Minute: m, Samples: 12, CPUs: 24, CPUAvgPct: pf(20), CPUMaxPct: pf(80), Load1: 3,
			MemTotal: 20 << 30, MemAvailableMin: 5 << 30, DiskTotal: 100 << 30, DiskAvailableMin: 40 << 30,
			PSIMemSome: pf(1.5), OOMKills: pu(0), PaseoMemMax: pu(11 << 30), PaseoMemPeak: pu(13 << 30)}},
		Agents: []resources.AgentMinute{
			{AgentID: "a1", Minute: m, CPUSeconds: pf(30), CPUSource: "ebpf", MemMax: 2 << 30, ProcessesMax: 4, Execs: pu(120)},
			{AgentID: "a2", Minute: m, CPUSeconds: pf(1), CPUSource: "ebpf", MemMax: 1 << 30, ProcessesMax: 1, Execs: pu(0)}},
		Probe: &resources.ProbeStatus{Running: true, Version: "v1", Tracked: 300},
	}
}

func TestRecordHostStoresMinutesAgentsKillsAndTheProbe(t *testing.T) {
	r := New(dbtestOwner(t))
	now := time.Now().UTC().Truncate(time.Minute)
	rep := hostReport(now.Add(-time.Minute))
	rep.OOMs = []probe.OOM{{Seq: 1, At: now.Add(-30 * time.Second), PID: 77, Comm: "node", AgentID: "a1", AnonRSS: 9 << 30}}
	if err := r.RecordHost(ctx, "host-1", rep, now); err != nil {
		t.Fatal(err)
	}
	// the same minutes again (a retried send) replace, not duplicate
	if err := r.RecordHost(ctx, "host-1", rep, now); err != nil {
		t.Fatal(err)
	}
	var minutes, agents, kills int
	if err := r.p.QueryRow(ctx, `SELECT (SELECT count(*) FROM host_minutes), (SELECT count(*) FROM agent_minutes),
		(SELECT count(*) FROM oom_kills)`).Scan(&minutes, &agents, &kills); err != nil {
		t.Fatal(err)
	}
	if minutes != 1 || agents != 2 || kills != 1 {
		t.Errorf("rows: %d minutes, %d agents, %d kills", minutes, agents, kills)
	}
	res, err := r.HostResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := res["host-1"]
	if h.Minute == nil || h.Minute.CPUs != 24 || h.MemUsedFrac < 0.74 || h.MemUsedFrac > 0.76 || !h.Fresh || h.Probe == nil || !h.Probe.Running ||
		h.Probe.Version != "v1" || h.AgentMem["a1"] != 2<<30 {
		t.Errorf("host = %+v (minute %+v)", h, h.Minute)
	}
	// a minute from before the probe ran: the agent still counts as measured by the probe
	early := resources.Report{Agents: []resources.AgentMinute{{AgentID: "a1", Minute: now.Add(-10 * time.Minute), CPUSeconds: pf(1), CPUSource: "proc", MemMax: 1}}}
	if err := r.RecordHost(ctx, "host-1", early, now); err != nil {
		t.Fatal(err)
	}
	top, err := r.TopAgents(ctx, "host-1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].AgentID != "a1" || top[0].CPUSeconds != 31 || top[0].Execs != 120 || top[0].MemMax != 2<<30 ||
		top[0].CPUSource != "ebpf" {
		t.Errorf("top = %+v", top)
	}
	ooms, err := r.OOMKills(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ooms) != 1 || ooms[0].Comm != "node" || ooms[0].AgentID != "a1" || ooms[0].Host != "host-1" {
		t.Errorf("ooms = %+v", ooms)
	}
	hist, err := r.HostHistory(ctx, "host-1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].CPUAvgPct == nil || *hist[0].CPUAvgPct != 20 {
		t.Errorf("history = %+v", hist)
	}
}

func TestPruneHostHistory(t *testing.T) {
	r := New(dbtestOwner(t))
	now := time.Now().UTC().Truncate(time.Minute)
	if err := r.RecordHost(ctx, "host-1", hostReport(now.Add(-100*24*time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordHost(ctx, "host-1", hostReport(now), now); err != nil {
		t.Fatal(err)
	}
	if err := r.PruneHosts(ctx, now.Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var minutes, agents int
	if err := r.p.QueryRow(ctx, "SELECT (SELECT count(*) FROM host_minutes), (SELECT count(*) FROM agent_minutes)").Scan(&minutes, &agents); err != nil {
		t.Fatal(err)
	}
	if minutes != 1 || agents != 2 {
		t.Errorf("after pruning: %d minutes, %d agent minutes", minutes, agents)
	}
}

func dbtestOwner(t *testing.T) *pgxpool.Pool { return dbtest.New(t).Owner }

func TestRecordHostStoresDiskIOLoadAndFilesystems(t *testing.T) {
	r := New(dbtestOwner(t))
	now := time.Now().UTC().Truncate(time.Minute)
	first := hostReport(now.Add(-2 * time.Minute))
	m := &first.Minutes[0]
	m.Load5, m.Load15 = 2, 1
	m.DiskReadBps, m.DiskWriteBps, m.DiskIOPS = pf(10<<20), pf(5<<20), pf(300)
	m.DiskReadBpsMax, m.DiskWriteBpsMax, m.DiskBusyAvgPct, m.DiskBusyMaxPct = pf(40<<20), pf(20<<20), pf(35), pf(90)
	m.Filesystems = []resources.Filesystem{
		{Mount: "/", Device: "/dev/sda1", Type: "ext4", Total: 100 << 30, Available: 40 << 30},
		{Mount: "/data", Device: "/dev/sdb1", Type: "xfs", Total: 1000 << 30, Available: 900 << 30}}
	if err := r.RecordHost(ctx, "host-1", first, now); err != nil {
		t.Fatal(err)
	}
	// the next minute's reading replaces the host's filesystems: /data was unmounted
	second := hostReport(now.Add(-time.Minute))
	second.Minutes[0].Filesystems = []resources.Filesystem{{Mount: "/", Device: "/dev/sda1", Type: "ext4", Total: 100 << 30, Available: 30 << 30}}
	if err := r.RecordHost(ctx, "host-1", second, now); err != nil {
		t.Fatal(err)
	}
	// an older reading sent late (a retry) does not undo a newer one
	if err := r.RecordHost(ctx, "host-1", first, now); err != nil {
		t.Fatal(err)
	}
	res, err := r.HostResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := res["host-1"]
	if len(h.Filesystems) != 1 || h.Filesystems[0].Mount != "/" || h.Filesystems[0].Available != 30<<30 || h.Filesystems[0].Type != "ext4" {
		t.Errorf("filesystems = %+v", h.Filesystems)
	}
	hist, err := r.HostHistory(ctx, "host-1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history = %+v", hist)
	}
	d := hist[0]
	if d.Load5 != 2 || d.Load15 != 1 || d.DiskReadBps == nil || *d.DiskReadBps != 10<<20 || *d.DiskWriteBps != 5<<20 || *d.DiskIOPS != 300 ||
		*d.DiskReadBpsMax != 40<<20 || *d.DiskWriteBpsMax != 20<<20 || *d.DiskBusyAvgPct != 35 || *d.DiskBusyMaxPct != 90 {
		t.Errorf("minute = %+v", d)
	}
	if hist[1].DiskReadBps != nil || hist[1].DiskBusyMaxPct != nil {
		t.Errorf("a minute without disk I/O has some: %+v", hist[1])
	}
	// SQL reads it all: load per CPU now, the day's busiest disk and bytes moved, each filesystem's use
	var loadPerCPU, busyMax, readBytes, usedFrac float64
	if err := r.p.QueryRow(ctx, `SELECT (SELECT load1_per_cpu FROM host_current WHERE host = 'host-1'),
		(SELECT disk_busy_max_pct FROM host_days WHERE host = 'host-1'), (SELECT disk_read_bytes FROM host_days WHERE host = 'host-1'),
		(SELECT used_frac FROM host_filesystems WHERE host = 'host-1' AND mount = '/')`).Scan(&loadPerCPU, &busyMax, &readBytes, &usedFrac); err != nil {
		t.Fatal(err)
	}
	if loadPerCPU != 0.125 || busyMax != 90 || readBytes != 60*(10<<20) || usedFrac != 0.7 {
		t.Errorf("sql: load per CPU %v, busiest %v, read %v, used %v", loadPerCPU, busyMax, readBytes, usedFrac)
	}
}
