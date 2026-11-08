package resources

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, root, path, content string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statLine is /proc/<pid>/stat with utime and stime (fields 14 and 15) and starttime (22).
func statLine(pid, comm string, utime, stime, start int) string {
	return pid + " (" + comm + ") S 1 1 1 0 -1 4194560 100 0 0 0 " +
		itoa(utime) + " " + itoa(stime) + " 0 0 20 0 1 0 " + itoa(start) + " 1000 100 0"
}

func itoa(n int) string { return strconvItoa(n) }

func fakeProc(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	write(t, root, "loadavg", "1.50 0.75 0.25 2/300 999\n")
	write(t, root, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 100 kB\nSwapFree: 90 kB\n")
	write(t, root, "pressure/cpu", "some avg10=1.00 avg60=0 avg300=0 total=1\nfull avg10=0.00 avg60=0 avg300=0 total=0\n")
	write(t, root, "pressure/memory", "some avg10=2.00 avg60=0 avg300=0 total=1\nfull avg10=1.00 avg60=0 avg300=0 total=0\n")
	write(t, root, "pressure/io", "some avg10=3.00 avg60=0 avg300=0 total=1\nfull avg10=0.50 avg60=0 avg300=0 total=0\n")
	write(t, root, "vmstat", "nr_free_pages 1\noom_kill 4\n")
	// the Paseo daemon and two agents: a1 with two processes (one with smaps_rollup), a2 with one
	write(t, root, "100/cgroup", "0::/system.slice/paseo.service\n")
	write(t, root, "100/environ", "HOME=/h\x00")
	write(t, root, "100/stat", statLine("100", "Paseo Daemon", 5, 5, 10))
	write(t, root, "200/environ", "A=1\x00PASEO_AGENT_ID=a1\x00")
	write(t, root, "200/stat", statLine("200", "claude", 300, 100, 20))
	write(t, root, "200/smaps_rollup", "00400000-ff [rollup]\nRss: 9000 kB\nPss: 8000 kB\n")
	write(t, root, "201/environ", "PASEO_AGENT_ID=a1\x00")
	write(t, root, "201/stat", statLine("201", "node (dev server)", 50, 50, 30))
	write(t, root, "201/statm", "5000 1000 100 1 0 10 0\n")
	write(t, root, "300/environ", "PASEO_AGENT_ID=a2\x00")
	write(t, root, "300/stat", statLine("300", "codex", 10, 0, 40))
	write(t, root, "300/statm", "5000 500 100 1 0 10 0\n")
	write(t, root, "self/x", "") // not a process
	return root
}

func fakeCgroup(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "system.slice/paseo.service/memory.current", "300000\n")
	write(t, root, "system.slice/paseo.service/memory.peak", "500000\n")
	write(t, root, "system.slice/paseo.service/memory.stat", "anon 200000\nfile 100000\nkernel 1\n")
	write(t, root, "system.slice/paseo.service/memory.events", "low 0\nhigh 0\nmax 0\noom 1\noom_kill 2\n")
	return root
}

func TestLinuxSample(t *testing.T) {
	proc, cg := fakeProc(t), fakeCgroup(t)
	r := &Reader{Proc: proc, Cgroup: cg, Home: t.TempDir(), PaseoPID: func() int { return 100 }}
	s := r.Sample(time.Unix(100, 0))
	if s.CPUPct != nil {
		t.Errorf("first sample has CPU %% %v: it needs two", *s.CPUPct)
	}
	if s.MemTotal != 1000<<10 || s.MemAvailable != 400<<10 || s.SwapUsed != 10<<10 || s.Load != [3]float64{1.5, 0.75, 0.25} {
		t.Errorf("sample = %+v", s)
	}
	if s.PSICPUSome == nil || *s.PSICPUSome != 1 || *s.PSIMemFull != 1 || *s.PSIIOSome != 3 || s.OOMKills == nil || *s.OOMKills != 4 {
		t.Errorf("pressure and kills = %+v", s)
	}
	if s.PaseoMem == nil || *s.PaseoMem != 200000 || *s.PaseoMemPeak != 500000 || *s.PaseoOOMKills != 2 {
		t.Errorf("paseo cgroup = %v %v %v", s.PaseoMem, s.PaseoMemPeak, s.PaseoOOMKills)
	}
	if s.DiskTotal == 0 {
		t.Errorf("no disk")
	}
	write(t, proc, "stat", "cpu  150 0 150 900 0 0 0 0 0 0\n")
	if s := r.Sample(time.Unix(105, 0)); s.CPUPct == nil || *s.CPUPct != 50 {
		t.Errorf("second sample CPU = %v", s.CPUPct)
	}
}

func TestAgentProcessesAreGroupedByTheirAgent(t *testing.T) {
	proc := fakeProc(t)
	w := &AgentWalker{Proc: proc, PageSize: 4096}
	got := w.Walk()
	a1, a2 := got["a1"], got["a2"]
	// a1: 8000 kB PSS + 1000 pages; a2: 500 pages
	if a1.Processes != 2 || a1.Mem != 8000<<10+1000*4096 || a2.Processes != 1 || a2.Mem != 500*4096 || len(got) != 2 {
		t.Fatalf("walk = %+v", got)
	}
	if a1.CPUTicks != 0 {
		t.Errorf("first walk has CPU %v: it needs two", a1.CPUTicks)
	}
	// a1's claude used 40 more ticks; its dev server exited and a new process (same pid, new start) began
	write(t, proc, "200/stat", statLine("200", "claude", 330, 110, 20))
	write(t, proc, "201/stat", statLine("201", "node", 1, 1, 99))
	if got := w.Walk(); got["a1"].CPUTicks != 40 {
		t.Errorf("a1 ticks = %v", got["a1"].CPUTicks)
	}
}
