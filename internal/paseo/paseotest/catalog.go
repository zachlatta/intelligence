package paseotest

// CatalogEntry is a provider snapshot entry as get_providers_snapshot serves it: a ready
// provider with its models (each with its thinking options) and modes, as Paseo 0.15 sends them
// to a client without the compact snapshot capability.
func CatalogEntry(provider string) map[string]any {
	thinking := []any{map[string]any{"id": "low", "label": "Low"}, map[string]any{"id": "medium", "label": "Medium", "isDefault": true},
		map[string]any{"id": "high", "label": "High"}}
	return map[string]any{"provider": provider, "status": "ready", "enabled": true, "label": "Claude",
		"fetchedAt": "2026-11-08T07:00:00.000Z", "defaultModeId": "auto",
		"models": []any{
			map[string]any{"provider": provider, "id": "claude-opus-5-5", "label": "Opus 5.5", "description": "Latest release",
				"isDefault": true, "thinkingOptions": thinking, "defaultThinkingOptionId": "medium"},
			map[string]any{"provider": provider, "id": "claude-sonnet-5-5", "label": "Sonnet 5.5", "thinkingOptions": thinking,
				"defaultThinkingOptionId": "high"},
			map[string]any{"provider": provider, "id": "claude-hidden", "label": "Hidden", "isSelectable": false},
		},
		"modes": []any{
			map[string]any{"id": "auto", "label": "Auto mode", "icon": "ShieldCheck"},
			map[string]any{"id": "bypassPermissions", "label": "Bypass", "description": "No prompts"},
		}}
}

// SetCatalog sets the entries get_providers_snapshot serves (see CatalogEntry). Before it is
// called, the daemon serves one ready claude entry and an unavailable pi one.
func (f *Daemon) SetCatalog(e ...map[string]any) {
	f.mu.Lock()
	f.catalog = e
	f.catalogSet = true
	f.mu.Unlock()
}

func (f *Daemon) catalogLocked() []map[string]any {
	if f.catalogSet {
		return f.catalog
	}
	return []map[string]any{CatalogEntry("claude"), {"provider": "pi", "status": "unavailable", "enabled": true}}
}
