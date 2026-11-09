package paseo

import "context"

// ProviderEntry is one provider in the daemon's provider snapshot: whether it is ready, and then
// the models and modes an agent of it can be started with.
type ProviderEntry struct {
	Provider      string  `json:"provider"`
	Status        string  `json:"status"` // ready, loading, error, unavailable
	Enabled       *bool   `json:"enabled"`
	Error         string  `json:"error"`
	Label         string  `json:"label"`
	DefaultModeID string  `json:"defaultModeId"`
	Models        []Model `json:"models"`
	Modes         []Mode  `json:"modes"`
}

// Model is a model a provider offers, with the thinking levels it takes.
type Model struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Description     string   `json:"description"`
	IsDefault       bool     `json:"isDefault"`
	IsSelectable    *bool    `json:"isSelectable"`
	ThinkingOptions []Option `json:"thinkingOptions"`
	DefaultThinking string   `json:"defaultThinkingOptionId"`
}

// Mode is a provider's permission mode.
type Mode struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Option is one choice of a select, such as a thinking level.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	IsDefault   bool   `json:"isDefault"`
}

// ProviderCatalog reads the daemon's provider snapshot: each provider's models and modes. The
// daemon warms it in the background, so a provider can still be loading; read it again later.
// A model the daemon marks unselectable is left out.
func (c *Client) ProviderCatalog(ctx context.Context) ([]ProviderEntry, error) {
	var p struct {
		Entries []ProviderEntry `json:"entries"`
	}
	if err := c.request(ctx, map[string]any{"type": "get_providers_snapshot_request"}, &p); err != nil {
		return nil, err
	}
	for i := range p.Entries {
		var models []Model
		for _, m := range p.Entries[i].Models {
			if m.IsSelectable == nil || *m.IsSelectable {
				models = append(models, m)
			}
		}
		p.Entries[i].Models = models
	}
	return p.Entries, nil
}
