package host

import "time"

// Sample reads the host now. CPU % needs the sample before, so the first has none.
func (r *Reader) Sample(now time.Time) Sample { return r.sampleLinux(now) }
