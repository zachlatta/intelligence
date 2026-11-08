package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/devbox"
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
	return New(Config{Version: "c0ffee", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret", BinDir: dir}), dir
}

func TestEveryResponseNamesTheServerVersion(t *testing.T) {
	h, _ := binServer(t)
	for _, path := range []string{"/", "/healthz", "/api/devboxes", "/api/sql?q=select+1", "/nope"} {
		if got := get(t, h, path, nil).Header().Get(devbox.VersionHeader); got != "c0ffee" {
			t.Errorf("%s: %s = %q, want c0ffee", path, devbox.VersionHeader, got)
		}
	}
	if got := post(t, h, devbox.ReportPath, "wrong", []byte("{}")).Header().Get(devbox.VersionHeader); got != "c0ffee" {
		t.Errorf("refused report: version header = %q", got)
	}
}

func TestServesTheDevboxBinaryWithVersionAndChecksum(t *testing.T) {
	h, _ := binServer(t)
	rec := get(t, h, devbox.BinaryPath("linux", "amd64"), nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF fake" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
	sum := sha256.Sum256([]byte("ELF fake"))
	if got := rec.Header().Get(devbox.SHA256Header); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 header = %q", got)
	}
	if got := rec.Header().Get(devbox.VersionHeader); got != "c0ffee" {
		t.Errorf("version header = %q", got)
	}
}

func TestUnknownPlatformsAndOddPathsAre404(t *testing.T) {
	h, _ := binServer(t)
	for _, path := range []string{
		devbox.BinaryPath("plan9", "amd64"),
		"/api/devbox/binary/../secret.txt",
		"/api/devbox/binary/secret.txt/x",
		"/api/devbox/binary/linux/amd64.",
	} {
		if rec := get(t, h, path, nil); rec.Code == http.StatusOK {
			t.Errorf("%s: status 200, want not found", path)
		}
	}
}
