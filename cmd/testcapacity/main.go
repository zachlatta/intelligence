// testcapacity runs one suite under a host-wide two-slot lease.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/zachlatta/intelligence/internal/testcapacity"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testcapacity command [args...]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// /var/lock is shared by all users and worktrees on Linux. On macOS the
	// host must provision this shared directory once before running heavy suites.
	dir := os.Getenv("INTELLIGENCE_TEST_CAPACITY_DIR")
	if dir == "" {
		dir = "/var/lock"
	}
	lease, err := testcapacity.Acquire(ctx, dir, func() { fmt.Fprintln(os.Stderr, "waiting for test capacity (2 host-wide slots)") })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lease.Close()
	fmt.Fprintln(os.Stderr, "test capacity acquired")
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{lease}
	cmd.Env = append(os.Environ(), "INTELLIGENCE_TEST_CAPACITY_HELD=1")
	// The child inherits the lease too, so killing this wrapper does not admit
	// another suite while that child is still alive. No other suite is signaled.
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		err = <-done
	case err = <-done:
	}
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}
