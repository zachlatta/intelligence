package workflow

import "testing"

func TestSoloAdmissionHasAPhysicalBound(t *testing.T) {
	const g = int64(1 << 30)
	c := capacity{limits: DefaultLimits(), m: minute{MemTotal: 20 * g, MemMin: 12 * g}}
	if c.fits(candidate{Memory: 12 * g}, false) == "" {
		t.Fatal("solo bypass admitted an estimate above half the host")
	}
	c.m.MemMin = 3 * g
	if c.fits(candidate{Memory: 4 * g}, false) == "" {
		t.Fatal("solo bypass exceeded the peak ceiling")
	}
	c.m.MemMin = 4*g + g/2
	if got := c.fits(candidate{Memory: g}, false); got != "" {
		t.Fatalf("bounded solo exit lost: %s", got)
	}
	c.runs = []hostRun{{ID: "waiting", Waiting: true, Estimate: 10 * g, Resident: g}}
	if got := c.fits(candidate{Memory: g}, false); got != "" {
		t.Fatalf("waiting run retained unconsumed reservation: %s", got)
	}
}

func TestReadmissionSubtractsItsOwnMeasuredUse(t *testing.T) {
	const g = int64(1 << 30)
	c := capacity{limits: DefaultLimits(), m: minute{MemTotal: 20 * g, MemMin: 5 * g}, runs: []hostRun{{ID: "resume", Waiting: true, Estimate: 8 * g, Resident: 8 * g}}}
	if got := c.fits(candidate{ID: "resume", Memory: 8 * g}, false); got != "" {
		t.Fatalf("measured use charged twice on readmission: %s", got)
	}
}

func TestAdmissionPreservesMainOverlappingPeakRule(t *testing.T) {
	const g = int64(1 << 30)
	c := capacity{limits: DefaultLimits(), m: minute{MemTotal: 20 * g, MemMin: 14 * g}, runs: []hostRun{
		{ID: "a", Active: true, Estimate: 6 * g, Resident: 2 * g},
		{ID: "b", Active: true, Estimate: 6 * g, Resident: 2 * g},
		{ID: "c", Active: true, Estimate: 6 * g, Resident: 2 * g},
	}}
	if got := c.fits(candidate{Memory: g}, false); got != "" {
		t.Fatalf("reserved every concurrent peak again: %s", got)
	}
}
