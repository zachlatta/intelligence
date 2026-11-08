package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/host"
	"github.com/zachlatta/intelligence/internal/probe"
)

func seedHost(t *testing.T, d dbtest.DB) {
	t.Helper()
	sendReport(t, New(Config{Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret"}), sampleReport)
	now := time.Now().UTC().Truncate(time.Minute)
	f := func(v float64) *float64 { return &v }
	u := func(v uint64) *uint64 { return &v }
	reg := fleet.New(d.Owner)
	for i := 30; i >= 1; i-- {
		m := now.Add(-time.Duration(i) * time.Minute)
		rep := host.Report{
			Minutes: []host.Minute{{Minute: m, Samples: 12, CPUs: 24, CPUAvgPct: f(float64(i)), CPUMaxPct: f(90), Load1: 2,
				MemTotal: 20 << 30, MemAvailableMin: 8 << 30, DiskTotal: 100 << 30, DiskAvailableMin: 25 << 30,
				PSIMemSome: f(0.5), OOMKills: u(0), PaseoMemMax: u(11 << 30)}},
			Agents: []host.AgentMinute{{AgentID: agentA1, Minute: m, CPUSeconds: f(6), CPUSource: "ebpf", MemMax: 3 << 30, ProcessesMax: 4, Execs: u(10)}},
			Probe:  &host.ProbeStatus{Running: true, Version: "v", Tracked: 99},
		}
		if i == 1 {
			rep.OOMs = []probe.OOM{{At: m, PID: 42, Comm: "node", AgentID: agentA1, AnonRSS: 9 << 30}}
		}
		if err := reg.RecordHost(t.Context(), "devbox-1", rep, m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndexShowsEachHostsResourcesAndAgentsMemory(t *testing.T) {
	h, d := dbServer(t)
	seedHost(t, d)
	body := get(t, h, "/", nil).Body.String()
	for _, want := range []string{`href="/hosts#devbox-1"`, "CPU 1%", "RAM 12.0/20.0 GB", "disk 75%", "3.0 GB"} {
		if !strings.Contains(body, want) {
			t.Errorf("index misses %q", want)
		}
	}
}

func TestHostsPageChartsEachBoxAndRanksAgents(t *testing.T) {
	h, d := dbServer(t)
	seedHost(t, d)
	rec := get(t, h, "/hosts", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="devbox-1"`, "<polyline", "Fix the", "3m 0s", "300 commands", "OOM kills", "node", "9.0 GB",
		"eBPF probe v", "Paseo"} {
		if !strings.Contains(body, want) {
			t.Errorf("hosts page misses %q", want)
		}
	}
	if rec := get(t, h, "/hosts?hours=0", nil); rec.Code != 400 {
		t.Errorf("hours=0: %d", rec.Code)
	}
}

func TestHostsAPI(t *testing.T) {
	h, d := dbServer(t)
	seedHost(t, d)
	if body := get(t, h, "/api/hosts", nil).Body.String(); !strings.Contains(body, `"devbox-1"`) || !strings.Contains(body, `"cpus":24`) {
		t.Errorf("api = %s", body)
	}
}
