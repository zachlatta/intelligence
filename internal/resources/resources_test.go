package resources

import (
	"reflect"
	"testing"
	"time"
)

func TestParseCPU(t *testing.T) {
	c, err := parseCPU("cpu  100 5 50 800 20 1 2 3 9 9\ncpu0 1 2 3\n")
	if err != nil {
		t.Fatal(err)
	}
	// guest times are already in user and nice; idle is idle + iowait
	if c != (cpuTicks{total: 981, idle: 820}) {
		t.Errorf("cpu = %+v", c)
	}
	if _, err := parseCPU("intr 1 2"); err == nil {
		t.Error("not a cpu line parsed")
	}
}

func TestCPUPercent(t *testing.T) {
	if p := cpuPercent(cpuTicks{1000, 800}, cpuTicks{1100, 850}); p == nil || *p != 50 {
		t.Errorf("busy = %v", p)
	}
	if p := cpuPercent(cpuTicks{1000, 800}, cpuTicks{900, 700}); p != nil {
		t.Errorf("counters went back: %v", *p)
	}
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo("MemTotal:       20000 kB\nMemFree: 100 kB\nMemAvailable:   5000 kB\nSwapTotal: 8000 kB\nSwapFree: 6000 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if m != (meminfo{memTotal: 20000 << 10, memAvailable: 5000 << 10, swapTotal: 8000 << 10, swapFree: 6000 << 10}) {
		t.Errorf("meminfo = %+v", m)
	}
	if _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Error("no MemAvailable parsed")
	}
}

func TestParsePressure(t *testing.T) {
	some, full, err := parsePressure("some avg10=1.50 avg60=0.40 avg300=0.10 total=123\nfull avg10=0.25 avg60=0.00 avg300=0.00 total=9\n")
	if err != nil || some != 1.5 || full == nil || *full != 0.25 {
		t.Errorf("pressure = %v %v %v", some, full, err)
	}
	some, full, err = parsePressure("some avg10=3.00 avg60=0 avg300=0 total=1\n") // cpu has no full line on older kernels
	if err != nil || some != 3 || full != nil {
		t.Errorf("cpu pressure = %v %v %v", some, full, err)
	}
}

func TestParseVmstatOOMKills(t *testing.T) {
	if n, ok := parseKeyed("nr_free_pages 5\noom_kill 3\n", "oom_kill"); !ok || n != 3 {
		t.Errorf("oom_kill = %d %v", n, ok)
	}
	if _, ok := parseKeyed("nr_free_pages 5\n", "oom_kill"); ok {
		t.Error("missing counter found")
	}
}

func TestCgroupPath(t *testing.T) {
	if p := cgroupPath("0::/system.slice/paseo.service\n"); p != "/system.slice/paseo.service" {
		t.Errorf("path = %q", p)
	}
	if p := cgroupPath("12:memory:/x\n"); p != "" {
		t.Errorf("cgroup v1 path = %q", p)
	}
}

func TestMinuteAggregatesSamples(t *testing.T) {
	m0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	u := func(v uint64) *uint64 { return &v }
	samples := []Sample{
		{At: m0.Add(5 * time.Second), CPUs: 4, CPUPct: f(10), Load: [3]float64{1, 2, 3}, MemTotal: 100, MemAvailable: 60,
			SwapTotal: 50, SwapUsed: 5, DiskTotal: 1000, DiskAvailable: 400, PSIMemSome: f(0.5), OOMKills: u(7),
			PaseoMem: u(30), PaseoMemPeak: u(40), PaseoOOMKills: u(1)},
		{At: m0.Add(10 * time.Second), CPUs: 4, CPUPct: f(30), Load: [3]float64{2, 2, 3}, MemTotal: 100, MemAvailable: 40,
			SwapTotal: 50, SwapUsed: 9, DiskTotal: 1000, DiskAvailable: 390, PSIMemSome: f(2.5), OOMKills: u(9),
			PaseoMem: u(35), PaseoMemPeak: u(45), PaseoOOMKills: u(2)},
	}
	got := aggregate(m0, samples, Sample{OOMKills: u(6), PaseoOOMKills: u(1)})
	want := Minute{Minute: m0, Samples: 2, CPUs: 4, CPUAvgPct: f(20), CPUMaxPct: f(30), Load1: 2, Load5: 2, Load15: 3,
		MemTotal: 100, MemAvailableMin: 40, SwapTotal: 50, SwapUsedMax: 9, DiskTotal: 1000, DiskAvailableMin: 390,
		PSIMemSome: f(2.5), OOMKills: u(3), PaseoMemMax: u(35), PaseoMemPeak: u(45), PaseoOOMKills: u(1)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("minute =\n%+v\nwant\n%+v", got, want)
	}
	// with no earlier sample, a counter's change counts from the minute's first sample
	if m := aggregate(m0, samples, Sample{}); m.OOMKills == nil || *m.OOMKills != 2 {
		t.Errorf("first minute's kills = %v", m.OOMKills)
	}
}
