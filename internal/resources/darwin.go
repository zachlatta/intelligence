package resources

import (
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
)

// Parsers and process attribution helpers for macOS (sample_darwin.go, walk_darwin.go).
// They build everywhere, so they are tested on Linux.

func parseDarwinLoad(text string) ([3]float64, bool) {
	var l [3]float64
	f := strings.Fields(strings.Trim(strings.TrimSpace(text), "{}"))
	if len(f) < 3 {
		return l, false
	}
	for i := range l {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return l, false
		}
		l[i] = v
	}
	return l, true
}

var pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat estimates available memory: free, inactive and speculative pages.
func parseVMStat(text string) (uint64, bool) {
	m := pageSizeRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	page, _ := strconv.ParseUint(m[1], 10, 64)
	var pages uint64
	seen := 0
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || (k != "Pages free" && k != "Pages inactive" && k != "Pages speculative") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil {
			return 0, false
		}
		pages += n
		seen++
	}
	return pages * page, seen == 3 && page > 0
}

var swapRe = regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)

func parseSwapUsage(text string) (total, used uint64, ok bool) {
	m := swapRe.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	t, e1 := strconv.ParseFloat(m[1], 64)
	u, e2 := strconv.ParseFloat(m[2], 64)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return uint64(t * (1 << 20)), uint64(u * (1 << 20)), true
}

// parseIostatCPU reads the last line of `iostat -c 2 -w 1 -n 0`: us sy id, then load.
func parseIostatCPU(text string) (float64, bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 3 {
		return 0, false
	}
	idle, err := strconv.ParseFloat(f[2], 64)
	if err != nil || idle < 0 || idle > 100 {
		return 0, false
	}
	return 100 - idle, true
}

// parseDarwinProcArgs2 extracts PASEO_AGENT_ID from a Darwin KERN_PROCARGS2 buffer.
// Layout: int32 argc, exec_path\0, NUL padding, argc NUL-terminated argv strings,
// followed by NUL-terminated envp KEY=VALUE strings.
func parseDarwinProcArgs2(buf []byte) string {
	if len(buf) < 4 {
		return ""
	}
	argc := int(int32(binary.LittleEndian.Uint32(buf[:4])))
	if argc < 0 {
		return ""
	}
	i := 4
	// Skip exec_path up to its terminating NUL.
	for i < len(buf) && buf[i] != 0 {
		i++
	}
	if i >= len(buf) {
		return ""
	}
	// Skip NUL padding between exec_path and argv[0].
	for i < len(buf) && buf[i] == 0 {
		i++
	}
	// Skip argc argument strings.
	for arg := 0; arg < argc && i < len(buf); arg++ {
		for i < len(buf) && buf[i] != 0 {
			i++
		}
		if i < len(buf) {
			i++
		}
	}
	if i >= len(buf) {
		return ""
	}
	return agentOf(buf[i:])
}

// machToTicks converts Mach absolute time units to 1/100 s CPU ticks (matching Linux USER_HZ)
// without overflowing uint64 for long-running processes.
func machToTicks(machTime, tbFreq uint64) uint64 {
	if tbFreq == 0 {
		return 0
	}
	return (machTime/tbFreq)*100 + (machTime%tbFreq)*100/tbFreq
}

type darwinProc struct {
	pid     int
	ppid    int
	start   uint64
	agentID string
	mem     uint64
	ticks   uint64
	hasTask bool
}

type darwinCachedAgent struct {
	start   uint64
	agentID string
}

// resolveDarwinAgents attributes processes without a direct PASEO_AGENT_ID to the nearest
// ancestor in procs that has one, guarding against PPID cycles.
func resolveDarwinAgents(procs []darwinProc) {
	byPID := make(map[int]int, len(procs))
	for i := range procs {
		byPID[procs[i].pid] = i
	}
	// state: 0 = unresolved, 1 = visiting (cycle guard), 2 = resolved
	state := make([]uint8, len(procs))
	var resolve func(idx int) string
	resolve = func(idx int) string {
		if procs[idx].agentID != "" {
			state[idx] = 2
			return procs[idx].agentID
		}
		if state[idx] != 0 {
			return procs[idx].agentID
		}
		state[idx] = 1
		ppid := procs[idx].ppid
		if ppid > 0 && ppid != procs[idx].pid {
			if pidx, ok := byPID[ppid]; ok {
				procs[idx].agentID = resolve(pidx)
			}
		}
		state[idx] = 2
		return procs[idx].agentID
	}
	for i := range procs {
		if state[i] == 0 {
			resolve(i)
		}
	}
}

func aggregateDarwinProcs(procs []darwinProc, prev map[int]procTicks) (map[string]AgentProcs, map[int]procTicks) {
	out := map[string]AgentProcs{}
	next := map[int]procTicks{}
	for _, p := range procs {
		if p.agentID == "" {
			continue
		}
		a := out[p.agentID]
		a.Processes++
		a.Mem += p.mem
		if p.hasTask {
			t := procTicks{start: p.start, ticks: p.ticks}
			next[p.pid] = t
			if old, ok := prev[p.pid]; ok && old.start == t.start && t.ticks >= old.ticks {
				a.CPUTicks += t.ticks - old.ticks
			}
		}
		out[p.agentID] = a
	}
	return out, next
}
