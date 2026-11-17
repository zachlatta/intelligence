package resources

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Sample reads the host now with macOS's tools. It has no pressure, cgroups or OOM counter.
func (r *Reader) Sample(now time.Time) Sample {
	s := Sample{At: now}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	run := func(name string, args ...string) string {
		out, _ := exec.CommandContext(ctx, name, args...).Output()
		return string(out)
	}
	s.CPUs, _ = strconv.Atoi(strings.TrimSpace(run("sysctl", "-n", "hw.ncpu")))
	if l, ok := parseDarwinLoad(run("sysctl", "-n", "vm.loadavg")); ok {
		s.Load = l
	}
	if total, err := strconv.ParseUint(strings.TrimSpace(run("sysctl", "-n", "hw.memsize")), 10, 64); err == nil {
		s.MemTotal = total
		if avail, ok := parseVMStat(run("vm_stat")); ok && avail <= total {
			s.MemAvailable = avail
		}
	}
	if total, used, ok := parseSwapUsage(run("sysctl", "-n", "vm.swapusage")); ok {
		s.SwapTotal, s.SwapUsed = total, used
	}
	if p, ok := parseIostatCPU(run("iostat", "-c", "2", "-w", "1", "-n", "0")); ok {
		s.CPUPct = &p
	}
	s.DiskTotal, s.DiskAvailable = diskSpace(r.Home)
	if text := run("ioreg", "-r", "-c", "IOBlockStorageDriver", "-w", "0"); text != "" {
		r.setDiskIO(now, parseDarwinIoreg(text), &s)
	}
	if r.filesystemsDue(now) {
		if fs := parseDarwinDf(run("df", "-kPl")); fs != nil {
			s.Filesystems = fs
		}
	}
	return s
}
