package testcapacity

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestCapacityIsSharedAndReleased(t *testing.T) {
	synctest.Test(t, testCapacityIsSharedAndReleased)
}

func testCapacityIsSharedAndReleased(t *testing.T) {
	// Keep test scratch inside the repository, including during CI.
	if err := os.MkdirAll("../../.factory-scratch", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("../../.factory-scratch", "capacity-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	first, err := Acquire(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Acquire(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{}, 1)
	acquired := make(chan *os.File, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		f, err := Acquire(ctx, dir, func() { waited <- struct{}{} })
		if err == nil {
			acquired <- f
		}
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		t.Fatal("no visible waiting state")
	}
	select {
	case f := <-acquired:
		f.Close()
		t.Fatal("third suite admitted")
	default:
	}
	second.Close()
	select {
	case f := <-acquired:
		f.Close()
	case <-ctx.Done():
		t.Fatal("released lease not reused")
	}
	// A second caller using an absolute path shares the same host-local lock files.
	abs, _ := filepath.Abs(dir)
	second, err = Acquire(ctx, abs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	canceled, stop := context.WithCancel(ctx)
	stop()
	if f, err := Acquire(canceled, dir, nil); err == nil {
		f.Close()
		t.Fatal("canceled waiter acquired capacity")
	}
}

func TestLeaseSurvivesWrapperExitAndReleasesAfterChildExit(t *testing.T) {
	if err := os.MkdirAll("../../.factory-scratch", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("../../.factory-scratch", "capacity-process-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	first, err := Acquire(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	lease, err := Acquire(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLeaseChild$")
	child.Env = append(os.Environ(), "TEST_CAPACITY_CHILD=1")
	child.ExtraFiles = []*os.File{lease}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("child failed to inherit lease")
	}
	lease.Close() // the wrapper can exit without releasing its child's lease
	ctx, cancel := context.WithCancel(t.Context())
	waited := false
	if f, err := Acquire(ctx, dir, func() { waited = true; cancel() }); err == nil {
		f.Close()
		t.Fatal("lease lost while child alive")
	}
	cancel()
	if !waited {
		t.Fatal("child's inherited lease did not produce the waiting state")
	}
	child.Process.Kill()
	child.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, err := Acquire(ctx, dir, nil)
	if err != nil {
		t.Fatal("crashed child leaked lease:", err)
	}
	f.Close()
}

func TestLeaseChild(t *testing.T) {
	if os.Getenv("TEST_CAPACITY_CHILD") != "1" {
		return
	}
	f := os.NewFile(3, "lease")
	if _, err := f.Stat(); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("ready\n")
	var b [1]byte
	os.Stdin.Read(b[:])
	os.Exit(0)
}
