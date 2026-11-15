// Package testcapacity bounds heavyweight test suites across worktrees on one host.
// These are local process leases, not application state. The kernel releases them on exit.
package testcapacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const Slots = 2

// Acquire waits for one of two shared advisory locks. Callers must keep the returned
// descriptor open for their entire suite and pass it to subprocesses as an ExtraFile.
// The callback reports the waiting state once, before the first wait.
func Acquire(ctx context.Context, dir string, waiting func()) (*os.File, error) {
	reported := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for slot := 0; slot < Slots; slot++ {
			path := filepath.Join(dir, fmt.Sprintf("intelligence-heavy-test-%d.lock", slot))
			// Read-only descriptors permit callers of different users to share the same
			// local lock. Do not unlink lock files: that would create separate lock domains.
			f, err := openLease(path)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			if !info.Mode().IsRegular() {
				f.Close()
				return nil, fmt.Errorf("test lease is not a regular file")
			}
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return f, nil
			}
			f.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				return nil, err
			}
		}
		if !reported {
			if waiting != nil {
				waiting()
			}
			reported = true
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Open existing files without O_CREATE: protected_regular rejects nonexclusive
// creation opens of another user's file in a shared sticky directory, even read-only.
func openLease(path string) (*os.File, error) {
	f, err := os.Open(path)
	if !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0444)
	if errors.Is(err, os.ErrExist) {
		return os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	// All users must be able to read new leases, even with a restrictive creator umask.
	if err := f.Chmod(0444); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
