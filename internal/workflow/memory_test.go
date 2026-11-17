package workflow

import "testing"

func TestMemoryAdmissionCountsEveryUnconsumedReservation(t *testing.T) {
	const g = int64(1 << 30)
	c := capacity{limits: DefaultLimits(), m: minute{MemTotal: 20 * g, MemMin: 14 * g}, runs: []hostRun{
		{ID: "a", Active: true, Estimate: 6 * g, Resident: 2 * g},
		{ID: "b", Active: true, Estimate: 6 * g, Resident: 2 * g},
		{ID: "c", Active: true, Estimate: 6 * g, Resident: 2 * g},
	}}
	k := candidate{Memory: g}
	if got := c.fits(k, false); got == "" {
		t.Fatal("admitted above 90% after counting all three reservations")
	}
	c.runs[2].Active, c.runs[2].Waiting = false, true
	if got := c.fits(k, false); got != "" {
		t.Fatalf("waiting run reserved more than its measured use: %s", got)
	}
	c.runs[0].Resident = 8 * g // growth beyond estimate cannot produce negative headroom
	c.m.MemMin = 8 * g
	if got := c.fits(k, false); got != "" {
		t.Fatalf("resident memory charged twice: %s", got)
	}
	c.runs = nil
	c.m.MemMin = 4 * g
	if got := c.fits(candidate{Memory: 3 * g}, false); got == "" {
		t.Fatal("oversized solo run bypassed memory safety limit")
	}
	c.m.MemMin = 10 * g
	c.runs = []hostRun{{ID: "retry", Active: true, Estimate: 6 * g, Resident: 4 * g}}
	if got := c.fits(candidate{ID: "retry", Memory: 6 * g}, false); got != "" {
		t.Fatalf("candidate's own measured use counted twice: %s", got)
	}
	c.runs[0].Resident = 0 // missing sample conservatively retains the whole reservation
	if got := c.fits(candidate{Memory: 2 * g}, false); got == "" {
		t.Fatal("admitted at exactly 90%, or dropped a retry's reservation")
	}
}
