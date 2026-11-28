package resources

import (
	"bytes"
	"encoding/binary"
	"testing"
)

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

func makeProcArgs2(execPath string, padNuls int, argv, envp []string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, int32(len(argv)))
	b.WriteString(execPath)
	b.WriteByte(0)
	for range padNuls {
		b.WriteByte(0)
	}
	for _, a := range argv {
		b.WriteString(a)
		b.WriteByte(0)
	}
	for _, e := range envp {
		b.WriteString(e)
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestParseDarwinProcArgs2(t *testing.T) {
	buf := makeProcArgs2("/usr/local/bin/node", 5,
		[]string{"node", "server.js", "PASEO_AGENT_ID=not-env"},
		[]string{"HOME=/Users/someone", "PASEO_AGENT_ID=agent-42", "PATH=/usr/bin"},
	)
	if got := parseDarwinProcArgs2(buf); got != "agent-42" {
		t.Errorf("got %q, want agent-42", got)
	}

	// Argv containing PASEO_AGENT_ID= must not be mistaken for envp.
	argOnly := makeProcArgs2("/usr/bin/env", 2,
		[]string{"env", "PASEO_AGENT_ID=from-arg", "true"},
		[]string{"HOME=/Users/someone"},
	)
	if got := parseDarwinProcArgs2(argOnly); got != "" {
		t.Errorf("arg-only got %q, want empty", got)
	}

	// Empty argv element and argc == 0.
	withEmptyArg := makeProcArgs2("/bin/sh", 1, []string{"sh", "", "-c"}, []string{"PASEO_AGENT_ID=a1"})
	if got := parseDarwinProcArgs2(withEmptyArg); got != "a1" {
		t.Errorf("empty arg got %q, want a1", got)
	}
	zeroArgs := makeProcArgs2("/bin/sh", 3, nil, []string{"PASEO_AGENT_ID=a0"})
	if got := parseDarwinProcArgs2(zeroArgs); got != "a0" {
		t.Errorf("zero args got %q, want a0", got)
	}

	// Malformed or truncated buffers must not panic.
	for _, bad := range [][]byte{nil, {}, {1, 2}, {0xff, 0xff, 0xff, 0xff, 'a', 0}, {1, 0, 0, 0, 'a', 'b'}} {
		if got := parseDarwinProcArgs2(bad); got != "" {
			t.Errorf("bad %x: got %q, want empty", bad, got)
		}
	}

	// Large (>64 KiB) argv + envp payload must parse cleanly without truncation or misreading argv.
	bigArg := string(bytes.Repeat([]byte{'x'}, 80<<10))
	bigBuf := makeProcArgs2("/usr/local/bin/claude", 4,
		[]string{"claude", "--prompt", bigArg, "PASEO_AGENT_ID=from-big-arg"},
		[]string{"HOME=/Users/someone", "BIG_ENV=" + string(bytes.Repeat([]byte{'y'}, 16<<10)), "PASEO_AGENT_ID=agent-big"},
	)
	if len(bigBuf) <= 64<<10 {
		t.Fatalf("expected payload > 64 KiB, got %d", len(bigBuf))
	}
	if got := parseDarwinProcArgs2(bigBuf); got != "agent-big" {
		t.Errorf("large payload got %q, want agent-big", got)
	}
}

func TestDarwinProcArgsBufSizeAndTruncation(t *testing.T) {
	// Buffer must be sized to kern.argmax (e.g. 1 MiB), not capped at 64 KiB.
	if got := darwinProcArgsBufSize(1<<20, true); got != 1<<20 {
		t.Errorf("argmax=1MiB: got %d, want %d", got, 1<<20)
	}
	if got := darwinProcArgsBufSize(256<<10, true); got != 256<<10 {
		t.Errorf("argmax=256KiB: got %d, want %d", got, 256<<10)
	}
	// Invalid or missing kern.argmax falls back to 1 MiB.
	if got := darwinProcArgsBufSize(0, false); got != 1<<20 {
		t.Errorf("missing argmax: got %d, want %d", got, 1<<20)
	}
	if got := darwinProcArgsBufSize(1024, true); got != 1<<20 {
		t.Errorf("tiny argmax: got %d, want %d", got, 1<<20)
	}

	// A full-buffer return (n == len(buf)) may be tail-truncated by XNU sysctl_procargsx
	// and must be rejected rather than parsed from a corrupt offset.
	buf := make([]byte, 128)
	payload := makeProcArgs2("/bin/sh", 2, []string{"sh", "-c"}, []string{"PASEO_AGENT_ID=a1"})
	copy(buf, payload)
	if _, ok := darwinProcArgsSlice(buf, len(buf)); ok {
		t.Errorf("n == len(buf) should be rejected as potentially truncated")
	}
	if _, ok := darwinProcArgsSlice(buf, 0); ok {
		t.Errorf("n == 0 should be rejected")
	}
	if got, ok := darwinProcArgsSlice(buf, len(payload)); !ok || parseDarwinProcArgs2(got) != "a1" {
		t.Errorf("valid slice got (%d bytes, %v), want ok with agent a1", len(got), ok)
	}
}

func TestMachToTicks(t *testing.T) {
	// Apple Silicon: 24 MHz timebase.
	if got := machToTicks(36_000_000, 24_000_000); got != 150 {
		t.Errorf("arm64 1.5s = %d, want 150", got)
	}
	// Intel: 1 GHz timebase.
	if got := machToTicks(1_500_000_000, 1_000_000_000); got != 150 {
		t.Errorf("amd64 1.5s = %d, want 150", got)
	}
	// Large value must not overflow uint64.
	const billionSec = uint64(1_000_000_000)
	if got := machToTicks(billionSec*1_000_000_000, 1_000_000_000); got != billionSec*100 {
		t.Errorf("large = %d, want %d", got, billionSec*100)
	}
	if got := machToTicks(100, 0); got != 0 {
		t.Errorf("zero freq = %d, want 0", got)
	}
}

func TestResolveDarwinAgentsAndAggregate(t *testing.T) {
	procs := []darwinProc{
		{pid: 50, ppid: 1, start: 10, mem: 100, ticks: 5},                                    // unrelated
		{pid: 100, ppid: 50, start: 20, agentID: "a1", mem: 1000, ticks: 200, hasTask: true}, // agent root
		{pid: 101, ppid: 100, start: 21, mem: 500, ticks: 50, hasTask: true},                 // child with cleared env
		{pid: 102, ppid: 101, start: 22, mem: 250, ticks: 10, hasTask: true},                 // grandchild
		{pid: 103, ppid: 100, start: 23, agentID: "a2", mem: 800, ticks: 40, hasTask: true},  // subagent
		{pid: 104, ppid: 103, start: 24, mem: 200, ticks: 15, hasTask: true},                 // subagent child
		{pid: 200, ppid: 201, start: 30, mem: 10, ticks: 1, hasTask: true},                   // cycle guard
		{pid: 201, ppid: 200, start: 31, mem: 10, ticks: 1, hasTask: true},                   // cycle guard
	}
	resolveDarwinAgents(procs)
	got1, prev := aggregateDarwinProcs(procs, nil)
	if len(got1) != 2 {
		t.Fatalf("walk1 = %+v, want 2 agents", got1)
	}
	if a1 := got1["a1"]; a1.Processes != 3 || a1.Mem != 1750 || a1.CPUTicks != 0 {
		t.Errorf("walk1 a1 = %+v", a1)
	}
	if a2 := got1["a2"]; a2.Processes != 2 || a2.Mem != 1000 || a2.CPUTicks != 0 {
		t.Errorf("walk1 a2 = %+v", a2)
	}

	// Second walk: PID 100 used 35 more ticks, PID 101 reused with new start time, PID 102 used 5 more ticks.
	procs2 := []darwinProc{
		{pid: 100, ppid: 50, start: 20, agentID: "a1", mem: 1100, ticks: 235, hasTask: true},
		{pid: 101, ppid: 100, start: 99, mem: 400, ticks: 10, hasTask: true}, // PID reused
		{pid: 102, ppid: 101, start: 22, mem: 250, ticks: 15, hasTask: true},
		{pid: 103, ppid: 100, start: 23, agentID: "a2", mem: 800, ticks: 60, hasTask: true},
	}
	resolveDarwinAgents(procs2)
	got2, _ := aggregateDarwinProcs(procs2, prev)
	if a1 := got2["a1"]; a1.Processes != 3 || a1.Mem != 1750 || a1.CPUTicks != 40 {
		t.Errorf("walk2 a1 = %+v, want 3 procs, 1750 mem, 40 ticks", a1)
	}
	if a2 := got2["a2"]; a2.Processes != 1 || a2.Mem != 800 || a2.CPUTicks != 20 {
		t.Errorf("walk2 a2 = %+v, want 1 proc, 800 mem, 20 ticks", a2)
	}
}
