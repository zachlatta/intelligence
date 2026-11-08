package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func binServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intelligence-linux-amd64"), []byte("ELF fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := dbtest.New(t)
	return newServer(t, d, d.Owner, "c0ffee", func(c *Config) { c.BinDir = dir }), dir
}

func TestEveryResponseNamesTheServerVersion(t *testing.T) {
	h, _ := binServer(t)
	for _, path := range []string{"/", "/healthz", "/api/hosts", "/api/sql?q=select+1", "/nope"} {
		if got := get(t, h, path, nil).Header().Get(hostclient.VersionHeader); got != "c0ffee" {
			t.Errorf("%s: %s = %q, want c0ffee", path, hostclient.VersionHeader, got)
		}
	}
	if got := get(t, h, hostclient.StreamPath("host-1"), map[string]string{"Authorization": "Bearer wrong"}).Header().Get(hostclient.VersionHeader); got != "c0ffee" {
		t.Errorf("refused stream: version header = %q", got)
	}
}

func TestServesTheHostBinaryWithVersionAndChecksum(t *testing.T) {
	h, _ := binServer(t)
	rec := get(t, h, hostclient.BinaryPath("linux", "amd64"), nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF fake" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
	sum := sha256.Sum256([]byte("ELF fake"))
	if got := rec.Header().Get(hostclient.SHA256Header); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 header = %q", got)
	}
	if got := rec.Header().Get(hostclient.VersionHeader); got != "c0ffee" {
		t.Errorf("version header = %q", got)
	}
}

func TestUnknownPlatformsAndOddPathsAre404(t *testing.T) {
	h, _ := binServer(t)
	for _, path := range []string{
		hostclient.BinaryPath("plan9", "amd64"),
		"/api/host/binary/../secret.txt",
		"/api/host/binary/secret.txt/x",
		"/api/host/binary/linux/amd64.",
	} {
		if rec := get(t, h, path, nil); rec.Code == http.StatusOK {
			t.Errorf("%s: status 200, want not found", path)
		}
	}
}

// A host client from before dev boxes were called hosts downloads its update from the old path.
func TestServesTheHostBinaryOnItsPathFromBeforeTheRename(t *testing.T) {
	h, _ := binServer(t)
	rec := get(t, h, "/api/devbox/binary/linux/amd64", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF fake" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
}
