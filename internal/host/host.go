// Package host measures a dev box: the host itself every few seconds (CPU, load, memory, swap,
// disk, pressure, OOM kills, the Paseo daemon's cgroup), each Paseo agent's processes (their
// memory and CPU), and, with the root probe (internal/probe), each agent's exact CPU and the
// processes the kernel killed for memory. The box sends a summary of each minute to the server.
package host

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Sample is one reading of the host. A nil pointer is a value this host does not have (macOS has
// no pressure or cgroups) or could not read.
type Sample struct {
	At            time.Time
	CPUs          int
	CPUPct        *float64 // busy since the sample before
	Load          [3]float64
	MemTotal      uint64
	MemAvailable  uint64
	SwapTotal     uint64
	SwapUsed      uint64
	DiskTotal     uint64 // the home filesystem
	DiskAvailable uint64
	PSICPUSome    *float64 // pressure: % of the last 10s some tasks waited for CPU
	PSIMemSome    *float64
	PSIMemFull    *float64
	PSIIOSome     *float64
	PSIIOFull     *float64
	OOMKills      *uint64 // the kernel's count of OOM kills since boot
	PaseoMem      *uint64 // the Paseo daemon's cgroup: memory now, its peak, its OOM kills
	PaseoMemPeak  *uint64
	PaseoOOMKills *uint64
}

// Minute is a minute of samples: averages, extremes, and the change in the kernel's counters.
type Minute struct {
	Minute           time.Time `json:"minute"`
	Samples          int       `json:"samples"`
	CPUs             int       `json:"cpus"`
	CPUAvgPct        *float64  `json:"cpuAvgPct,omitempty"`
	CPUMaxPct        *float64  `json:"cpuMaxPct,omitempty"`
	Load1            float64   `json:"load1"`
	Load5            float64   `json:"load5"`
	Load15           float64   `json:"load15"`
	MemTotal         uint64    `json:"memTotal"`
	MemAvailableMin  uint64    `json:"memAvailableMin"`
	SwapTotal        uint64    `json:"swapTotal"`
	SwapUsedMax      uint64    `json:"swapUsedMax"`
	DiskTotal        uint64    `json:"diskTotal"`
	DiskAvailableMin uint64    `json:"diskAvailableMin"`
	PSICPUSome       *float64  `json:"psiCpuSome,omitempty"` // the most of the minute's 10-second averages
	PSIMemSome       *float64  `json:"psiMemSome,omitempty"`
	PSIMemFull       *float64  `json:"psiMemFull,omitempty"`
	PSIIOSome        *float64  `json:"psiIoSome,omitempty"`
	PSIIOFull        *float64  `json:"psiIoFull,omitempty"`
	OOMKills         *uint64   `json:"oomKills,omitempty"` // during the minute
	PaseoMemMax      *uint64   `json:"paseoMemMax,omitempty"`
	PaseoMemPeak     *uint64   `json:"paseoMemPeak,omitempty"`
	PaseoOOMKills    *uint64   `json:"paseoOomKills,omitempty"` // during the minute
}

// aggregate summarizes the samples of the minute starting at m; before is the last sample of an
// earlier minute (zero when there is none), for the counters' change.
func aggregate(m time.Time, samples []Sample, before Sample) Minute {
	out := Minute{Minute: m, Samples: len(samples)}
	if len(samples) == 0 {
		return out
	}
	var cpuSum float64
	var cpuN int
	out.MemAvailableMin, out.DiskAvailableMin = math.MaxUint64, math.MaxUint64
	for _, s := range samples {
		out.CPUs = s.CPUs
		if s.CPUPct != nil {
			cpuSum += *s.CPUPct
			cpuN++
			out.CPUMaxPct = maxF(out.CPUMaxPct, s.CPUPct)
		}
		out.Load1, out.Load5, out.Load15 = s.Load[0], s.Load[1], s.Load[2]
		out.MemTotal, out.SwapTotal, out.DiskTotal = s.MemTotal, s.SwapTotal, s.DiskTotal
		out.MemAvailableMin = min(out.MemAvailableMin, s.MemAvailable)
		out.DiskAvailableMin = min(out.DiskAvailableMin, s.DiskAvailable)
		out.SwapUsedMax = max(out.SwapUsedMax, s.SwapUsed)
		out.PSICPUSome = maxF(out.PSICPUSome, s.PSICPUSome)
		out.PSIMemSome = maxF(out.PSIMemSome, s.PSIMemSome)
		out.PSIMemFull = maxF(out.PSIMemFull, s.PSIMemFull)
		out.PSIIOSome = maxF(out.PSIIOSome, s.PSIIOSome)
		out.PSIIOFull = maxF(out.PSIIOFull, s.PSIIOFull)
		out.PaseoMemMax = maxU(out.PaseoMemMax, s.PaseoMem)
		out.PaseoMemPeak = maxU(out.PaseoMemPeak, s.PaseoMemPeak)
	}
	if cpuN > 0 {
		avg := cpuSum / float64(cpuN)
		out.CPUAvgPct = &avg
	}
	last := samples[len(samples)-1]
	out.OOMKills = counterChange(before.OOMKills, samples[0].OOMKills, last.OOMKills)
	out.PaseoOOMKills = counterChange(before.PaseoOOMKills, samples[0].PaseoOOMKills, last.PaseoOOMKills)
	return out
}

// counterChange is how much a counter rose to last: from before, or else from first.
func counterChange(before, first, last *uint64) *uint64 {
	from := before
	if from == nil {
		from = first
	}
	if from == nil || last == nil || *last < *from {
		return nil // unknown, or reset (a reboot, a restarted service)
	}
	d := *last - *from
	return &d
}

func maxF(a, b *float64) *float64 {
	if b == nil || (a != nil && *a >= *b) {
		return a
	}
	v := *b
	return &v
}

func maxU(a, b *uint64) *uint64 {
	if b == nil || (a != nil && *a >= *b) {
		return a
	}
	v := *b
	return &v
}

var errFormat = errors.New("unexpected format")

type cpuTicks struct{ total, idle uint64 }

// parseCPU reads the first line of /proc/stat.
func parseCPU(text string) (cpuTicks, error) {
	line, _, _ := strings.Cut(text, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTicks{}, errFormat
	}
	var c cpuTicks
	// guest and guest_nice (fields 9 and 10) are already counted in user and nice
	for i := 1; i < len(f) && i <= 8; i++ {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return cpuTicks{}, errFormat
		}
		c.total += n
		if i == 4 || i == 5 { // idle, iowait
			c.idle += n
		}
	}
	return c, nil
}

func cpuPercent(prev, next cpuTicks) *float64 {
	if next.total <= prev.total || next.idle < prev.idle || next.idle-prev.idle > next.total-prev.total {
		return nil
	}
	p := 100 * float64((next.total-prev.total)-(next.idle-prev.idle)) / float64(next.total-prev.total)
	return &p
}

type meminfo struct{ memTotal, memAvailable, swapTotal, swapFree uint64 }

func parseMeminfo(text string) (meminfo, error) {
	var m meminfo
	seen := 0
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return m, errFormat
		}
		switch f[0] {
		case "MemTotal:":
			m.memTotal, seen = n<<10, seen|1
		case "MemAvailable:":
			m.memAvailable, seen = n<<10, seen|2
		case "SwapTotal:":
			m.swapTotal = n << 10
		case "SwapFree:":
			m.swapFree = n << 10
		}
	}
	if seen != 3 || m.memAvailable > m.memTotal || m.swapFree > m.swapTotal {
		return m, errFormat
	}
	return m, nil
}

// parsePressure reads a /proc/pressure file: the "some" and "full" 10-second averages.
func parsePressure(text string) (some float64, full *float64, err error) {
	found := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[1], "avg10=") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimPrefix(f[1], "avg10="), 64)
		if err != nil {
			return 0, nil, errFormat
		}
		switch f[0] {
		case "some":
			some, found = v, true
		case "full":
			full = &v
		}
	}
	if !found {
		return 0, nil, errFormat
	}
	return some, full, nil
}

// parseKeyed finds "key value" in a file of such lines (/proc/vmstat, memory.events).
func parseKeyed(text, key string) (uint64, bool) {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, " ")
		if ok && k == key {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// cgroupPath is a process's cgroup v2 path, from /proc/<pid>/cgroup.
func cgroupPath(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}
