package resources

import "testing"

func TestParseDarwin(t *testing.T) {
	if l, ok := parseDarwinLoad("{ 1.50 1.20 1.00 }\n"); !ok || l != [3]float64{1.5, 1.2, 1} {
		t.Errorf("load = %v %v", l, ok)
	}
	avail, ok := parseVMStat("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:        100.\nPages active: 5.\nPages inactive:     200.\nPages speculative:  50.\n")
	if !ok || avail != 350*16384 {
		t.Errorf("available = %v %v", avail, ok)
	}
	total, used, ok := parseSwapUsage("total = 2048.00M  used = 1024.50M  free = 1023.50M  (encrypted)\n")
	if !ok || total != 2048<<20 || used != uint64(1024.5*(1<<20)) {
		t.Errorf("swap = %v %v %v", total, used, ok)
	}
	if p, ok := parseIostatCPU("   us sy id   1m   5m   15m\n    5  3 92  1.5 1.2 1.0\n   10 10 80  1.5 1.2 1.0\n"); !ok || p != 20 {
		t.Errorf("cpu = %v %v", p, ok)
	}
}
