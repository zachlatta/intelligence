package hostclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The server names its version on every response, and serves the host binary it was built with.
const (
	VersionHeader = "X-Intelligence-Version"
	SHA256Header  = "X-Intelligence-Sha256"
)

// BinaryPath is where the server serves the host binary for one platform.
func BinaryPath(goos, goarch string) string { return "/api/host/binary/" + goos + "/" + goarch }

// UpdateEvery is the most often a host tries to update.
const UpdateEvery = time.Minute

// maxBinary bounds a downloaded binary.
const maxBinary = 200 << 20

// Updater keeps a host on the server's version: after an API call shows the server runs another
// version, it downloads the server's binary, checks it, replaces its own, and re-executes itself.
type Updater struct {
	Server   string // the server's base URL
	Client   *http.Client
	Current  string // this binary's version
	Exe      string // this binary's path, replaced by an update
	OS, Arch string // the platform to download
	Now      func() time.Time
	// Verify runs a downloaded binary and returns the version it reports (`<path> version`).
	Verify func(ctx context.Context, path string) (string, error)
	// Exec replaces this process with the binary at path (syscall.Exec); it returns only on failure.
	Exec func(path string) error

	mu   sync.Mutex
	last time.Time
}

// Maybe updates to serverVersion when it differs from Current, at most once per UpdateEvery.
// It returns only when no update ran or the update failed; a successful one re-executes.
func (u *Updater) Maybe(ctx context.Context, serverVersion string) error {
	if serverVersion == "" || serverVersion == u.Current {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.Now()
	if !u.last.IsZero() && now.Sub(u.last) < UpdateEvery {
		return nil
	}
	u.last = now
	if err := u.update(ctx, serverVersion); err != nil {
		return fmt.Errorf("update %s -> %s: %w", short(u.Current), short(serverVersion), err)
	}
	return nil
}

func (u *Updater) update(ctx context.Context, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.Server, "/")+BinaryPath(u.OS, u.Arch), nil)
	if err != nil {
		return err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	if got := resp.Header.Get(VersionHeader); got != want {
		return fmt.Errorf("download is version %q, not %q", got, want)
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Exe), filepath.Base(u.Exe)+".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; removes a failed download
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBinary+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if n > maxBinary {
		return fmt.Errorf("download is larger than %d bytes", maxBinary)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, resp.Header.Get(SHA256Header)) {
		return fmt.Errorf("download sha256 %s does not match the server's %q", sum, resp.Header.Get(SHA256Header))
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	got, err := u.Verify(ctx, tmp.Name())
	if err != nil {
		return fmt.Errorf("new binary does not run: %w", err)
	}
	if strings.TrimSpace(got) != want {
		return fmt.Errorf("new binary says it is version %q, not %q", strings.TrimSpace(got), want)
	}
	if err := os.Rename(tmp.Name(), u.Exe); err != nil {
		return err
	}
	return u.Exec(u.Exe)
}

func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}
