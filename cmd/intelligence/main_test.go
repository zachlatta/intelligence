package main

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestVersionPrefersTheStampedCommit(t *testing.T) {
	old := buildCommit
	t.Cleanup(func() { buildCommit = old })
	buildCommit = "stamped"
	if got := version(env(map[string]string{"SOURCE_COMMIT": "deadbeef"})); got != "stamped" {
		t.Fatalf("version = %q, want the commit stamped at build", got)
	}
}

func TestVersionPrefersSourceCommit(t *testing.T) {
	if got := version(env(map[string]string{"SOURCE_COMMIT": "deadbeef"})); got != "deadbeef" {
		t.Fatalf("version = %q, want deadbeef", got)
	}
}

func TestVersionFallsBackWithoutSourceCommit(t *testing.T) {
	if got := version(env(nil)); got == "" {
		t.Fatal("version is empty without SOURCE_COMMIT")
	}
}

func TestAddrDefaultsToPort8080(t *testing.T) {
	if got := addr(env(nil)); got != ":8080" {
		t.Fatalf("addr = %q, want :8080", got)
	}
	if got := addr(env(map[string]string{"PORT": "9000"})); got != ":9000" {
		t.Fatalf("addr = %q, want :9000", got)
	}
}

func TestDevboxConfigNeedsOnlyTheServer(t *testing.T) {
	host := func() (string, error) { return "devbox-1", nil }
	if _, err := devboxConfig(env(map[string]string{}), host); err == nil || !strings.Contains(err.Error(), "DEVBOX_SERVER") {
		t.Errorf("err = %v, want DEVBOX_SERVER missing", err)
	}
	// a box installed before accounts still has DEVBOX_TOKEN in its env file: it starts, and joins
	if _, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "old"}), host); err != nil {
		t.Errorf("an old env file: %v", err)
	}
}

func TestDevboxConfigDefaults(t *testing.T) {
	c, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "HOME": "/home/someone"}), func() (string, error) { return "Devbox-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "devbox-1" || c.Interval != 15*time.Second || c.Server != "https://x" || c.KeyFile != "/home/someone/.config/intelligence/devbox.key" {
		t.Errorf("config = %+v", c)
	}
	c, _ = devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_KEY_FILE": "/etc/box.key"}), func() (string, error) { return "h", nil })
	if c.KeyFile != "/etc/box.key" {
		t.Errorf("DEVBOX_KEY_FILE: %q", c.KeyFile)
	}
}

func TestDevboxConfigOverrides(t *testing.T) {
	c, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t",
		"DEVBOX_NAME": "box-2", "DEVBOX_INTERVAL": "1m"}), func() (string, error) { return "h", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "box-2" || c.Interval != time.Minute {
		t.Errorf("config = %+v", c)
	}
	if _, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t", "DEVBOX_NAME": "bad name"}),
		func() (string, error) { return "h", nil }); err == nil {
		t.Errorf("an invalid DEVBOX_NAME should be refused")
	}
}

func TestServerConfigNeedsBothDatabaseURLs(t *testing.T) {
	if _, err := serverConfig(env(map[string]string{"READONLY_DATABASE_URL": "postgres://ro:p@db/app"})); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v, want DATABASE_URL missing", err)
	}
	if _, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app"})); err == nil || !strings.Contains(err.Error(), "READONLY_DATABASE_URL") {
		t.Errorf("err = %v, want READONLY_DATABASE_URL missing", err)
	}
	if _, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://db/app"})); err == nil {
		t.Errorf("a read-only URL without a user and password should be refused")
	}
}

func TestServerConfigReadsTheReadonlyRoleFromItsURL(t *testing.T) {
	c, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://intelligence_ro:s3cr%40t@db/app?sslmode=disable"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadonlyRole != "intelligence_ro" || c.ReadonlyPassword != "s3cr@t" || c.BinDir != "/usr/local/share/intelligence/devbox" {
		t.Errorf("config = %+v", c)
	}
}

func TestDevboxAutoUpdateCanBeTurnedOff(t *testing.T) {
	base := map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t"}
	c, _ := devboxConfig(env(base), func() (string, error) { return "h", nil })
	if !c.AutoUpdate {
		t.Errorf("auto-update should default on")
	}
	base["DEVBOX_AUTOUPDATE"] = "off"
	c, _ = devboxConfig(env(base), func() (string, error) { return "h", nil })
	if c.AutoUpdate {
		t.Errorf("DEVBOX_AUTOUPDATE=off should turn it off")
	}
}

func TestDevboxPaseoHome(t *testing.T) {
	base := map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t", "HOME": "/home/someone"}
	c, err := devboxConfig(env(base), func() (string, error) { return "h", nil })
	if err != nil || c.PaseoHome != "/home/someone/.paseo" {
		t.Errorf("PaseoHome = %q (err %v), want ~/.paseo", c.PaseoHome, err)
	}
	base["DEVBOX_PASEO_HOME"] = "/srv/paseo"
	if c, _ := devboxConfig(env(base), func() (string, error) { return "h", nil }); c.PaseoHome != "/srv/paseo" {
		t.Errorf("PaseoHome = %q, want the override", c.PaseoHome)
	}
}

func TestProbeConfig(t *testing.T) {
	c := probeConfig(env(map[string]string{"PROBE_SERVER": "https://x", "PROBE_USER": "dev"}))
	if c.Server != "https://x" || c.User != "dev" || c.Socket != "/run/intelligence/probe.sock" || !c.AutoUpdate {
		t.Errorf("config = %+v", c)
	}
	if c := probeConfig(env(map[string]string{"PROBE_AUTOUPDATE": "off", "PROBE_SOCKET": "/s"})); c.AutoUpdate || c.Socket != "/s" {
		t.Errorf("config = %+v", c)
	}
}

func TestDevboxProbeSocketDefault(t *testing.T) {
	c, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t"}), func() (string, error) { return "b", nil })
	if err != nil || c.ProbeSocket != "/run/intelligence/probe.sock" {
		t.Errorf("probe socket = %q, %v", c.ProbeSocket, err)
	}
}
