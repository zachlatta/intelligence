package main

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

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

func TestDevboxConfigNeedsServerAndToken(t *testing.T) {
	host := func() (string, error) { return "mew-dev", nil }
	if _, err := devboxConfig(env(map[string]string{"DEVBOX_TOKEN": "t"}), host); err == nil || !strings.Contains(err.Error(), "DEVBOX_SERVER") {
		t.Errorf("err = %v, want DEVBOX_SERVER missing", err)
	}
	if _, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x"}), host); err == nil || !strings.Contains(err.Error(), "DEVBOX_TOKEN") {
		t.Errorf("err = %v, want DEVBOX_TOKEN missing", err)
	}
}

func TestDevboxConfigDefaults(t *testing.T) {
	c, err := devboxConfig(env(map[string]string{"DEVBOX_SERVER": "https://x", "DEVBOX_TOKEN": "t"}), func() (string, error) { return "Mew-Dev", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "mew-dev" || c.Interval != 15*time.Second || c.Server != "https://x" || c.Token != "t" {
		t.Errorf("config = %+v", c)
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
