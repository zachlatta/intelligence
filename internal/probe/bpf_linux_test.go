//go:build linux

package probe

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestProbeChargesAnAgentsShortLivedCommands loads the real probe into the kernel. It needs root
// and a kernel that allows eBPF (not the image build's container), so it runs only when asked:
//
//	go test -c -o probe.test ./internal/probe && sudo INTELLIGENCE_EBPF_TEST=1 ./probe.test -test.run Probe -test.v
func TestProbeChargesAnAgentsShortLivedCommands(t *testing.T) {
	if os.Getenv("INTELLIGENCE_EBPF_TEST") != "1" || os.Geteuid() != 0 {
		t.Skip("loads eBPF programs: set INTELLIGENCE_EBPF_TEST=1 and run as root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := Start(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent := "probe-test-" + strconv.Itoa(os.Getpid())
	// an agent that waits to be tagged, starts 200 short programs, then burns CPU in short-lived
	// shells: all of it is charged, though each child exits before a /proc reader could see it
	cmd := exec.Command("/bin/sh", "-c", `sleep 0.5; i=0; while [ $i -lt 200 ]; do /bin/true; i=$((i+1)); done
		for j in 1 2 3 4; do /bin/sh -c 'i=0; while [ $i -lt 100000 ]; do i=$((i+1)); done'; done`)
	start := time.Now()
	cmd.Env = append(os.Environ(), "PASEO_AGENT_ID="+agent)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime() // the kernel's own count
	wall := time.Since(start)
	time.Sleep(200 * time.Millisecond)
	s := p.Snapshot(0)
	c := s.Agents[agent]
	t.Logf("agent: %d execs, %.3fs CPU by the probe, %.3fs by the kernel's rusage, %.3fs wall; %d tasks tagged",
		c.Execs, float64(c.CPUNanos)/1e9, cpu.Seconds(), wall.Seconds(), s.Tracked)
	if c.Execs < 200 {
		t.Errorf("execs = %d, want at least 200", c.Execs)
	}
	if got, want := time.Duration(c.CPUNanos), cpu; got < want*8/10 || got > want*12/10 {
		t.Errorf("cpu = %s, want within 20%% of the kernel's %s", got, want)
	}
	if s.Epoch == "" {
		t.Error("no epoch")
	}
}
