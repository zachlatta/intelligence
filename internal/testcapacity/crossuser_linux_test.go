//go:build linux

package testcapacity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCapacitySharedAcrossUsers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise distinct UIDs")
	}
	policy, err := os.ReadFile("/proc/sys/fs/protected_regular")
	if err != nil || strings.TrimSpace(string(policy)) == "0" {
		t.Skip("protected_regular not enabled")
	}
	if err := os.MkdirAll("../../.factory-scratch", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("../../.factory-scratch", "cross-user-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"intelligence-heavy-test-0.lock", "intelligence-heavy-test-1.lock"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 1001, 1001); err != nil {
			if errors.Is(err, syscall.EPERM) {
				t.Skip("requires chown capability")
			}
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	// Inherited descriptors avoid depending on the repository or Go build directory
	// being traversable by the other user. Only the shared sticky directory is tested.
	cmd := exec.Command("/proc/self/fd/3", "-test.run=^TestCapacityCrossUserChild$")
	cmd.ExtraFiles = []*os.File{binary, directory}
	cmd.Env = append(os.Environ(), "TEST_CAPACITY_CROSS_UID=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1002, Gid: 1002}}
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("requires setuid capability")
		}
		t.Fatalf("other user could not acquire shared lease: %v\n%s", err, out)
	}
}

func TestCapacityCrossUserChild(t *testing.T) {
	if os.Getenv("TEST_CAPACITY_CROSS_UID") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := Acquire(ctx, "/proc/self/fd/4", nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}
