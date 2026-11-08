//go:build !linux && !darwin

package resources

import "time"

// Sample reads only the disk on systems other than Linux and macOS.
func (r *Reader) Sample(now time.Time) Sample {
	s := Sample{At: now}
	s.DiskTotal, s.DiskAvailable = diskSpace(r.Home)
	return s
}
