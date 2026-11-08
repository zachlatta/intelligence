package fleet

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/host"
	"github.com/zachlatta/intelligence/internal/probe"
)

func pf(v float64) *float64 { return &v }
func pu(v uint64) *uint64   { return &v }

func hostReport(m time.Time) host.Report {
	return host.Report{
		Minutes: []host.Minute{{Minute: m, Samples: 12, CPUs: 24, CPUAvgPct: pf(20), CPUMaxPct: pf(80), Load1: 3,
			MemTotal: 20 << 30, MemAvailableMin: 5 << 30, DiskTotal: 100 << 30, DiskAvailableMin: 40 << 30,
			PSIMemSome: pf(1.5), OOMKills: pu(0), PaseoMemMax: pu(11 << 30), PaseoMemPeak: pu(13 << 30)}},
		Agents: []host.AgentMinute{
			{AgentID: "a1", Minute: m, CPUSeconds: pf(30), CPUSource: "ebpf", MemMax: 2 << 30, ProcessesMax: 4, Execs: pu(120)},
			{AgentID: "a2", Minute: m, CPUSeconds: pf(1), CPUSource: "ebpf", MemMax: 1 << 30, ProcessesMax: 1, Execs: pu(0)}},
		Probe: &host.ProbeStatus{Running: true, Version: "v1", Tracked: 300},
	}
}

func TestRecordHostStoresMinutesAgentsKillsAndTheProbe(t *testing.T) {
	r := New(dbtestOwner(t))
	now := time.Now().UTC().Truncate(time.Minute)
	rep := hostReport(now.Add(-time.Minute))
	rep.OOMs = []probe.OOM{{Seq: 1, At: now.Add(-30 * time.Second), PID: 77, Comm: "node", AgentID: "a1", AnonRSS: 9 << 30}}
	if err := r.RecordHost(ctx, "devbox-1", rep, now); err != nil {
		t.Fatal(err)
	}
	// the same minutes again (a retried send) replace, not duplicate
	if err := r.RecordHost(ctx, "devbox-1", rep, now); err != nil {
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
	hosts, err := r.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := hosts["devbox-1"]
	if h.Minute == nil || h.Minute.CPUs != 24 || h.MemUsedFrac < 0.74 || h.MemUsedFrac > 0.76 || !h.Fresh || h.Probe == nil || !h.Probe.Running ||
		h.Probe.Version != "v1" || h.AgentMem["a1"] != 2<<30 {
		t.Errorf("host = %+v (minute %+v)", h, h.Minute)
	}
	// a minute from before the probe ran: the agent still counts as measured by the probe
	early := host.Report{Agents: []host.AgentMinute{{AgentID: "a1", Minute: now.Add(-10 * time.Minute), CPUSeconds: pf(1), CPUSource: "proc", MemMax: 1}}}
	if err := r.RecordHost(ctx, "devbox-1", early, now); err != nil {
		t.Fatal(err)
	}
	top, err := r.TopAgents(ctx, "devbox-1", now.Add(-time.Hour))
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
	if len(ooms) != 1 || ooms[0].Comm != "node" || ooms[0].AgentID != "a1" || ooms[0].Box != "devbox-1" {
		t.Errorf("ooms = %+v", ooms)
	}
	hist, err := r.HostHistory(ctx, "devbox-1", now.Add(-time.Hour))
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
	if err := r.RecordHost(ctx, "devbox-1", hostReport(now.Add(-100*24*time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordHost(ctx, "devbox-1", hostReport(now), now); err != nil {
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
