package paseo_test

import (
	"github.com/zachlatta/intelligence/internal/paseo"
	"testing"
)

func TestUnattendedModesAndProviders(t *testing.T) {
	for provider, want := range map[string]string{"claude": "bypassPermissions", "codex": "full-access", "muse": "allowAll", "antigravity": "full-access", "unknown": ""} {
		if got := paseo.UnattendedMode(provider); got != want {
			t.Errorf("%s: %s, want %s", provider, got, want)
		}
	}
	ps := paseo.UnattendedProviders()
	if len(ps) != 4 || ps[0] != "claude" {
		t.Fatal(ps)
	}
	for _, provider := range ps {
		if paseo.UnattendedMode(provider) == "" {
			t.Fatal(provider)
		}
	}
	ps[0] = "changed"
	if paseo.UnattendedProviders()[0] != "claude" {
		t.Fatal("provider list is mutable")
	}
}
