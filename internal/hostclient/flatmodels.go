package hostclient

import "github.com/zachlatta/intelligence/internal/paseo"

// flatThinkingProviders are the providers whose Paseo daemons before 0.17 list each thinking
// level as a model of its own ("gemini-4-neon-medium", no thinking levels), where 0.17 lists one
// model with thinking levels ("gemini-4-neon", thinking "medium"). Plans and workflows name the
// flat models, so a host on a newer daemon lists them too and starts one as the model and level
// the daemon knows; hosts on either daemon then offer the same models.
var flatThinkingProviders = map[string]bool{"antigravity": true}

// withFlatModels is models with, for a flat-thinking provider, a flat model for each thinking
// level of each model that has them, after the daemon's own (none repeats one the daemon lists).
func withFlatModels(provider string, models []Model) []Model {
	if !flatThinkingProviders[provider] {
		return models
	}
	listed := map[string]bool{}
	for _, m := range models {
		listed[m.ID] = true
	}
	out := models
	for _, m := range models {
		for _, level := range m.Thinking {
			id := m.ID + "-" + level
			if listed[id] {
				continue
			}
			listed[id] = true
			out = append(out, Model{ID: id, Label: m.Label + " (" + level + ")", Description: m.Description})
		}
	}
	return out
}

// startModel is the model and thinking level to start an agent of provider on, given the ones
// asked for: a flat model the daemon does not list itself becomes its model and thinking level.
func startModel(catalog map[string]paseo.ProviderEntry, provider, model, thinking string) (string, string) {
	if !flatThinkingProviders[provider] || thinking != "" || model == "" {
		return model, thinking
	}
	e := catalog[provider]
	for _, m := range e.Models {
		if m.ID == model {
			return model, thinking
		}
	}
	for _, m := range e.Models {
		for _, o := range m.ThinkingOptions {
			if m.ID+"-"+o.ID == model {
				return m.ID, o.ID
			}
		}
	}
	return model, thinking
}

// flatAgentModel is the model and thinking level to report a running agent on: for a
// flat-thinking provider, the flat model of a model and level the daemon lists, so the agent
// matches the plans that name it.
func flatAgentModel(catalog map[string]paseo.ProviderEntry, provider, model, thinking string) (string, string) {
	if !flatThinkingProviders[provider] || thinking == "" {
		return model, thinking
	}
	for _, m := range catalog[provider].Models {
		if m.ID != model {
			continue
		}
		for _, o := range m.ThinkingOptions {
			if o.ID == thinking {
				return model + "-" + thinking, ""
			}
		}
	}
	return model, thinking
}

// startModel is startModel on the watcher's last catalog.
func (w *Watcher) startModel(provider, model, thinking string) (string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return startModel(w.catalog, provider, model, thinking)
}

// withStartModel is parsed with a schedule's agent on its start model (see startModel). An update
// that leaves the provider unchanged is passed as it is.
func (w *Watcher) withStartModel(parsed any) any {
	a, ok := parsed.(ScheduleArgs)
	if !ok || a.Agent == nil {
		return parsed
	}
	ag := *a.Agent
	ag.Model, ag.Thinking = w.startModel(ag.Provider, ag.Model, ag.Thinking)
	a.Agent = &ag
	return a
}
