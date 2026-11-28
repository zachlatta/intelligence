package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	h, _ := binServer(t)
	rec := get(t, h, "/api/devbox/binary/linux/amd64", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF fake" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
}

func TestServesInstallHostScriptWithDefaultServer(t *testing.T) {
	t.Parallel()
	h, _ := binServer(t)

	rec := get(t, anon(h), "/install-host", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "intel.example:8443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /install-host: status %d body %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if v := rec.Header().Get(hostclient.VersionHeader); v != "c0ffee" {
		t.Errorf("version header = %q, want c0ffee", v)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#!/usr/bin/env bash\n") {
		t.Errorf("script does not start with bash shebang")
	}
	if !strings.Contains(body, `: "${HOST_SERVER:=https://intel.example:8443}"`) {
		t.Errorf("script missing default HOST_SERVER:\n%s", body[:min(300, len(body))])
	}
}

func TestInstallHostScriptRejectsUnsafeOriginInjection(t *testing.T) {
	t.Parallel()
	h, _ := binServer(t)

	rec := get(t, anon(h), "/install-host", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "evil.example;id",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /install-host: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "evil.example") {
		t.Errorf("unsafe host header was injected into install-host script")
	}
}

func TestAccountPageShowsStandaloneInstallCommand(t *testing.T) {
	t.Parallel()
	h, _ := binServer(t)

	rec := get(t, h, "/account", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "intel.example:8443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account: status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "curl -fsSL https://intel.example:8443/install-host | bash") {
		t.Errorf("/account missing standalone curl install command")
	}
}

func TestInstallHostEndToEndFromServer(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	dir := t.TempDir()
	binBody := []byte("#!/bin/sh\necho c0ffee\n")
	if err := os.WriteFile(filepath.Join(dir, "intelligence-linux-amd64"), binBody, 0o755); err != nil {
		t.Fatal(err)
	}
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "c0ffee", func(c *Config) { c.BinDir = dir })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/install-host")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /install-host: %s", resp.Status)
	}

	home := t.TempDir()
	stubs := t.TempDir()
	writeStub := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub("uname", `case "${1:-}" in -m) echo x86_64;; *) echo Linux;; esac`)
	writeStub("systemctl", `exit 0`)
	writeStub("journalctl", `echo 'join: enter the code ZZZZ-9999 on the account page'; exit 0`)
	writeStub("sleep", `exit 0`)

	cmd := exec.Command("bash")
	cmd.Dir = t.TempDir()
	cmd.Stdin = resp.Body
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "run"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("piped install-host failed: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".config/intelligence/host.env")); err != nil || string(b) != "HOST_SERVER="+srv.URL+"\n" {
		t.Fatalf("host.env = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".local/bin/intelligence")); err != nil || string(b) != string(binBody) {
		t.Fatalf("binary = %q, %v", b, err)
	}
	if !strings.Contains(string(out), "enter the code ZZZZ-9999") {
		t.Errorf("output lacks enrollment code:\n%s", out)
	}
}
