package resources

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func strconvItoa(n int) string { return strconv.Itoa(n) }

// Reader samples a Linux host from /proc and the cgroup filesystem. Other systems: see
// read_darwin.go; on those Proc is unused.
type Reader struct {
	Proc     string     // default /proc
	Cgroup   string     // default /sys/fs/cgroup
	Home     string     // the filesystem whose space is measured
	PaseoPID func() int // the Paseo daemon, for its cgroup; 0 unknown

	prev *cpuTicks
}

func (r *Reader) proc(p ...string) string {
	root := r.Proc
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, p...)...)
}

func readString(path string) (string, bool) {
	b, err := os.ReadFile(path)
	return string(b), err == nil
}

// sampleLinux reads every Linux value it can; what it cannot read stays zero or nil.
func (r *Reader) sampleLinux(now time.Time) Sample {
	s := Sample{At: now}
	if text, ok := readString(r.proc("stat")); ok {
		if c, err := parseCPU(text); err == nil {
			if r.prev != nil {
				s.CPUPct = cpuPercent(*r.prev, c)
			}
			r.prev = &c
		}
		s.CPUs = strings.Count(text, "\ncpu")
	}
	if text, ok := readString(r.proc("loadavg")); ok {
		f := strings.Fields(text)
		for i := 0; i < 3 && i < len(f); i++ {
			s.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	if text, ok := readString(r.proc("meminfo")); ok {
		if m, err := parseMeminfo(text); err == nil {
			s.MemTotal, s.MemAvailable = m.memTotal, m.memAvailable
			s.SwapTotal, s.SwapUsed = m.swapTotal, m.swapTotal-m.swapFree
		}
	}
	pressure := func(name string) (*float64, *float64) {
		text, ok := readString(r.proc("pressure", name))
		if !ok {
			return nil, nil
		}
		some, full, err := parsePressure(text)
		if err != nil {
			return nil, nil
		}
		return &some, full
	}
	s.PSICPUSome, _ = pressure("cpu")
	s.PSIMemSome, s.PSIMemFull = pressure("memory")
	s.PSIIOSome, s.PSIIOFull = pressure("io")
	if text, ok := readString(r.proc("vmstat")); ok {
		if n, ok := parseKeyed(text, "oom_kill"); ok {
			s.OOMKills = &n
		}
	}
	s.DiskTotal, s.DiskAvailable = diskSpace(r.Home)
	if r.PaseoPID != nil {
		if pid := r.PaseoPID(); pid > 0 {
			r.paseoCgroup(pid, &s)
		}
	}
	return s
}

func (r *Reader) paseoCgroup(pid int, s *Sample) {
	text, ok := readString(r.proc(strconv.Itoa(pid), "cgroup"))
	if !ok {
		return
	}
	p := cgroupPath(text)
	if p == "" || p == "/" {
		return // not in a cgroup of its own: the root's numbers are the host's
	}
	root := r.Cgroup
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	dir := filepath.Join(root, filepath.Clean("/"+p))
	num := func(name string) *uint64 {
		text, ok := readString(filepath.Join(dir, name))
		if !ok {
			return nil
		}
		n, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	// memory.current counts the page cache its processes read; less that, it compares with the
	// host's used memory. The peak is the kernel's, cache included.
	s.PaseoMem, s.PaseoMemPeak = num("memory.current"), num("memory.peak")
	if s.PaseoMem != nil {
		if text, ok := readString(filepath.Join(dir, "memory.stat")); ok {
			if file, ok := parseKeyed(text, "file"); ok && file <= *s.PaseoMem {
				v := *s.PaseoMem - file
				s.PaseoMem = &v
			}
		}
	}
	if text, ok := readString(filepath.Join(dir, "memory.events")); ok {
		if n, ok := parseKeyed(text, "oom_kill"); ok {
			s.PaseoOOMKills = &n
		}
	}
}

func diskSpace(path string) (total, available uint64) {
	if path == "" {
		return 0, 0
	}
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil || st.Bsize <= 0 {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return uint64(st.Blocks) * bs, uint64(st.Bavail) * bs
}

// AgentProcs is one agent's processes now: how many, their memory (proportional set size where
// the kernel gives it, so shared pages count once), and CPU ticks used since the walk before.
type AgentProcs struct {
	Processes int
	Mem       uint64
	CPUTicks  uint64 // 1/100 s on Linux
}

// AgentWalker finds each Paseo agent's processes: every process whose environment names it
// (PASEO_AGENT_ID, which an agent's commands inherit). It reads only processes of its own user.
type AgentWalker struct {
	Proc     string // default /proc
	PageSize uint64 // default the system's

	prev map[int]procTicks
}

type procTicks struct{ start, ticks uint64 }

var agentVar = []byte("PASEO_AGENT_ID=")

// Walk returns every agent with a process now.
func (w *AgentWalker) Walk() map[string]AgentProcs {
	root := w.Proc
	if root == "" {
		root = "/proc"
	}
	page := w.PageSize
	if page == 0 {
		page = uint64(os.Getpagesize())
	}
	entries, _ := os.ReadDir(root)
	out := map[string]AgentProcs{}
	next := map[int]procTicks{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		env, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			continue // another user's, or gone
		}
		id := agentOf(env)
		if id == "" {
			continue
		}
		a := out[id]
		a.Processes++
		a.Mem += procMem(dir, page)
		if text, ok := readString(filepath.Join(dir, "stat")); ok {
			if t, ok := parseProcStat(text); ok {
				next[pid] = t
				if p, ok := w.prev[pid]; ok && p.start == t.start && t.ticks >= p.ticks {
					a.CPUTicks += t.ticks - p.ticks
				}
			}
		}
		out[id] = a
	}
	w.prev = next
	return out
}

func agentOf(env []byte) string {
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, agentVar); ok {
			return string(v)
		}
	}
	return ""
}

// procMem is a process's proportional set size, or its resident set where that is unreadable.
func procMem(dir string, page uint64) uint64 {
	if text, ok := readString(filepath.Join(dir, "smaps_rollup")); ok {
		for _, line := range strings.Split(text, "\n") {
			if v, ok := strings.CutPrefix(line, "Pss:"); ok {
				f := strings.Fields(v)
				if len(f) == 2 && f[1] == "kB" {
					if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
						return n << 10
					}
				}
			}
		}
	}
	if text, ok := readString(filepath.Join(dir, "statm")); ok {
		f := strings.Fields(text)
		if len(f) >= 2 {
			if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				return n * page
			}
		}
	}
	return 0
}

// parseProcStat reads utime + stime and the start time from /proc/<pid>/stat. The command name
// may hold spaces and parentheses, so fields count from the last ')'.
func parseProcStat(text string) (procTicks, bool) {
	i := strings.LastIndexByte(text, ')')
	if i < 0 {
		return procTicks{}, false
	}
	f := strings.Fields(text[i+1:])
	// after the name: state is field 3, so utime (14) is f[11], stime f[12], starttime (22) f[19]
	if len(f) < 20 {
		return procTicks{}, false
	}
	ut, e1 := strconv.ParseUint(f[11], 10, 64)
	st, e2 := strconv.ParseUint(f[12], 10, 64)
	start, e3 := strconv.ParseUint(f[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return procTicks{}, false
	}
	return procTicks{start: start, ticks: ut + st}, true
}
