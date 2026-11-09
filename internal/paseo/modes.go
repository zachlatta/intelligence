package paseo

// unattendedModes follows the providers' never-ask modes, in editor order.
var unattendedModes = [...]struct{ provider, mode string }{
	{"claude", "bypassPermissions"},
	{"codex", "full-access"},
	{"muse", "allowAll"},
	{"antigravity", "full-access"},
}

// UnattendedMode is the provider's mode that never asks, or "" for an unknown provider.
func UnattendedMode(provider string) string {
	for _, m := range unattendedModes {
		if m.provider == provider {
			return m.mode
		}
	}
	return ""
}

// UnattendedProviders returns the providers with never-ask modes, in editor order.
func UnattendedProviders() []string {
	ps := make([]string, len(unattendedModes))
	for i, m := range unattendedModes {
		ps[i] = m.provider
	}
	return ps
}
