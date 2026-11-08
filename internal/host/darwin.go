package host

import (
	"regexp"
	"strconv"
	"strings"
)

// Parsers for macOS's tools (sample_darwin.go). They build everywhere, so they are tested on Linux.

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
