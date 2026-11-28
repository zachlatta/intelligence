package hostclient

import (
	"slices"
	"testing"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// antigravityEntry is an Antigravity catalog as Paseo 0.17 lists it: one model with thinking
// levels where Paseo 0.15 listed one model per level ("gemini-4-neon-medium").
func antigravityEntry() map[string]any {
	thinking := []any{map[string]any{"id": "low"}, map[string]any{"id": "medium"}, map[string]any{"id": "high"}}
	return map[string]any{"provider": "antigravity", "status": "ready", "enabled": true, "label": "Antigravity",
		"defaultModeId": "full-access",
		"models": []any{
			map[string]any{"id": "gemini-4-neon", "label": "Gemini 4 Neon", "thinkingOptions": thinking, "defaultThinkingOptionId": "high"},
			map[string]any{"id": "gpt-oss-120b-medium", "label": "GPT-OSS 120B (Medium)"},
		}}
}

// TestAntigravityListsFlatThinkingModels: a host whose Paseo lists Antigravity models with
// thinking levels also lists each level as its own model, the way older Paseo daemons do, so a
// plan that names "gemini-4-neon-medium" is eligible on every host.
func TestAntigravityListsFlatThinkingModels(t *testing.T) {
	d := daemon(t)
	d.SetCatalog(paseotest.CatalogEntry("claude"), antigravityEntry())
	d.SetStatusProviders("claude", "antigravity")
	w := watcher(t, d)
	s := eventually(t, w, "a catalog", func(s PaseoStatus) bool { return len(s.Providers) == 2 && len(s.Providers[1].Models) > 0 })
	var ids []string
	for _, m := range s.Providers[1].Models {
		ids = append(ids, m.ID)
		if m.ID == "gemini-4-neon-medium" && (len(m.Thinking) != 0 || m.Label != "Gemini 4 Neon (medium)") {
			t.Errorf("flat model = %+v, want no thinking levels", m)
		}
	}
	want := []string{"gemini-4-neon", "gpt-oss-120b-medium", "gemini-4-neon-low", "gemini-4-neon-medium", "gemini-4-neon-high"}
	if !slices.Equal(ids, want) {
		t.Errorf("antigravity models = %v, want %v", ids, want)
	}
	if n := len(s.Providers[0].Models); n != 2 {
		t.Errorf("claude has %d models: only Antigravity gets flat ones", n)
	}
}

// TestAntigravityFlatModelStartsAsModelAndThinking: an agent or schedule asked for on a flat
// model starts on the model and thinking level the daemon lists; a model the daemon lists itself
// is passed through.
func TestAntigravityFlatModelStartsAsModelAndThinking(t *testing.T) {
	d := daemon(t)
	d.SetCatalog(paseotest.CatalogEntry("claude"), antigravityEntry())
	d.SetStatusProviders("claude", "antigravity")
	w := watcher(t, d)
	eventually(t, w, "a catalog", func(s PaseoStatus) bool { return len(s.Providers) == 2 && len(s.Providers[1].Models) > 0 })
	if _, err := execute(t, w, CreateAgentKind, `{"cwd":"/w/app","provider":"antigravity","model":"gemini-4-neon-medium","prompt":"go"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, w, CreateWorkspaceKind, `{"cwd":"/w/app","agent":{"provider":"antigravity","model":"gpt-oss-120b-medium","prompt":"go"}}`); err != nil {
		t.Fatal(err)
	}
	reqs := d.Requests("agent.create.request")
	if len(reqs) != 2 {
		t.Fatalf("%d agent requests", len(reqs))
	}
	if cfg := reqs[0]["config"].(map[string]any); cfg["model"] != "gemini-4-neon" || cfg["thinkingOptionId"] != "medium" {
		t.Errorf("config = %v, want gemini-4-neon at medium", cfg)
	}
	if cfg := reqs[1]["config"].(map[string]any); cfg["model"] != "gpt-oss-120b-medium" || cfg["thinkingOptionId"] != nil {
		t.Errorf("config = %v, want the listed model as it is", cfg)
	}

	if _, err := execute(t, w, CreateScheduleKind, `{"prompt":"go","every":"30m","agent":{"provider":"antigravity","cwd":"/w","model":"gemini-4-neon-high"}}`); err != nil {
		t.Fatal(err)
	}
	sreqs := d.Requests("schedule/create")
	if len(sreqs) != 1 {
		t.Fatalf("%d schedule requests", len(sreqs))
	}
	if cfg := sreqs[0]["target"].(map[string]any)["config"].(map[string]any); cfg["model"] != "gemini-4-neon" || cfg["thinkingOptionId"] != "high" {
		t.Errorf("schedule config = %v, want gemini-4-neon at high", cfg)
	}
}

// TestFlatAgentModel: a running Antigravity agent on a model and thinking level the daemon lists
// is reported on its flat model, so it matches plans; other agents are reported as they are.
func TestFlatAgentModel(t *testing.T) {
	cat := map[string]paseo.ProviderEntry{"antigravity": {Models: []paseo.Model{{ID: "gemini-4-neon",
		ThinkingOptions: []paseo.Option{{ID: "low"}, {ID: "medium"}}}}}}
	for _, c := range []struct{ provider, model, thinking, wantModel, wantThinking string }{
		{"antigravity", "gemini-4-neon", "medium", "gemini-4-neon-medium", ""},
		{"antigravity", "gemini-4-neon", "xhigh", "gemini-4-neon", "xhigh"},
		{"antigravity", "gemini-4-neon-medium", "", "gemini-4-neon-medium", ""},
		{"claude", "gemini-4-neon", "medium", "gemini-4-neon", "medium"},
	} {
		m, th := flatAgentModel(cat, c.provider, c.model, c.thinking)
		if m != c.wantModel || th != c.wantThinking {
			t.Errorf("flatAgentModel(%s, %s, %s) = %s, %s; want %s, %s", c.provider, c.model, c.thinking, m, th, c.wantModel, c.wantThinking)
		}
	}
}
