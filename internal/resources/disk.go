package resources

import (
	"regexp"
	"strconv"
	"strings"
)

// Filesystem is one local filesystem's space.
type Filesystem struct {
	Mount     string `json:"mount"`
	Device    string `json:"device,omitempty"`
	Type      string `json:"type,omitempty"`
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"` // to an ordinary user (less the root reserve)
}

// diskStat is a disk's counters since boot.
type diskStat struct {
	readBytes, writeBytes uint64
	ops                   uint64 // reads and writes done
	busyMs                uint64 // time the disk had I/O in flight (Linux's io_ticks)
	hasBusy               bool
}

// diskCounters is each disk's counters, by name.
type diskCounters map[string]diskStat

// parseDiskstats reads /proc/diskstats, keeping the disks whole says are whole physical disks, so
// partitions and the devices built on them (device mapper, RAID, loop) are not counted twice.
func parseDiskstats(text string, whole func(string) bool) diskCounters {
	out := diskCounters{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		// major minor name reads merged sectors ms writes merged sectors ms in-flight io_ticks ...
		if len(f) < 13 || !whole(f[2]) {
			continue
		}
		var n [13]uint64
		ok := true
		for _, i := range []int{3, 5, 7, 9, 12} {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok {
			continue
		}
		// sectors are 512 bytes in /proc/diskstats whatever the disk's own sector size
		out[f[2]] = diskStat{readBytes: n[5] * 512, writeBytes: n[9] * 512, ops: n[3] + n[7], busyMs: n[12], hasBusy: true}
	}
	return out
}

// diskIO is the host's disks over an interval: bytes and operations a second across them all, and
// the busiest disk's share of the time it had I/O in flight (nil where the system does not say).
type diskIO struct {
	readBps, writeBps, iops float64
	busyPct                 *float64
}

// diskRates is the disks' rates from prev to next, secs apart: nil without both readings. A disk
// counts only when it is in both and its counters did not go back (a reset).
func diskRates(prev, next diskCounters, secs float64) *diskIO {
	if prev == nil || next == nil || secs <= 0 {
		return nil
	}
	io := &diskIO{}
	for name, n := range next {
		p, ok := prev[name]
		if !ok || n.readBytes < p.readBytes || n.writeBytes < p.writeBytes || n.ops < p.ops {
			continue
		}
		io.readBps += float64(n.readBytes-p.readBytes) / secs
		io.writeBps += float64(n.writeBytes-p.writeBytes) / secs
		io.iops += float64(n.ops-p.ops) / secs
		if n.hasBusy && p.hasBusy && n.busyMs >= p.busyMs {
			b := min(100, 100*float64(n.busyMs-p.busyMs)/(secs*1000))
			io.busyPct = maxF(io.busyPct, &b)
		}
	}
	return io
}

type mount struct{ device, path, fsType string }

// localFS is the filesystem types whose space is measured: local disks only. Network filesystems
// are left out, as statfs on one whose server is gone can hang.
var localFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"apfs": true, "hfs": true,
}

// parseMounts reads /proc/self/mounts: each local filesystem once, at its first mount (a bind
// mount or a btrfs subvolume repeats its device; a ZFS pool's datasets share its space).
func parseMounts(text string) []mount {
	var out []mount
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !localFS[f[2]] {
			continue
		}
		key := f[0]
		if f[2] == "zfs" {
			key, _, _ = strings.Cut(f[0], "/")
			key = "zfs:" + key
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, mount{device: unescapeMount(f[0]), path: unescapeMount(f[1]), fsType: f[2]})
	}
	return out
}

// unescapeMount undoes the octal escapes (\040 for a space) in /proc/self/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseDarwinDf reads `df -kPl`: each disk volume. An APFS container's volumes share its space,
// so the system's own volumes are left out but for the Data volume, which holds what users store
// (and "/" with it, when there is one: "/" is then the read-only system snapshot).
func parseDarwinDf(text string) []Filesystem {
	var out []Filesystem
	hasData := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !strings.HasPrefix(f[0], "/dev/") {
			continue
		}
		total, e1 := strconv.ParseUint(f[1], 10, 64)
		avail, e2 := strconv.ParseUint(f[3], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		// the mount point is the rest of the line after the capacity, and may hold spaces
		path := strings.Join(f[5:], " ")
		if path == "/System/Volumes/Data" {
			hasData = true
		} else if strings.HasPrefix(path, "/System/Volumes/") || strings.HasPrefix(path, "/Library/Developer/CoreSimulator/") {
			// the system's volumes, and Xcode's read-only simulator runtime images (always full)
			continue
		}
		out = append(out, Filesystem{Mount: path, Device: f[0], Total: total << 10, Available: avail << 10})
	}
	if hasData {
		kept := out[:0]
		for _, fs := range out {
			if fs.Mount != "/" {
				kept = append(kept, fs)
			}
		}
		out = kept
	}
	return out
}

var ioregStatsRe = regexp.MustCompile(`"Statistics" = \{([^}]*)\}`)

// parseDarwinIoreg reads `ioreg -r -c IOBlockStorageDriver -w 0`: each disk's bytes and operations
// since boot, by its place in the list. macOS does not say how long a disk was busy.
func parseDarwinIoreg(text string) diskCounters {
	out := diskCounters{}
	for i, m := range ioregStatsRe.FindAllStringSubmatch(text, -1) {
		var d diskStat
		for _, kv := range strings.Split(m[1], ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				continue
			}
			switch strings.Trim(strings.TrimSpace(k), `"`) {
			case "Bytes (Read)":
				d.readBytes = n
			case "Bytes (Write)":
				d.writeBytes = n
			case "Operations (Read)", "Operations (Write)":
				d.ops += n
			}
		}
		out[strconv.Itoa(i)] = d
	}
	return out
}
