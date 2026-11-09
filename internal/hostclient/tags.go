package hostclient

import (
	"github.com/zachlatta/intelligence/internal/paseo"
)

// TagPaseoGUI tags a workspace made in the Paseo app (desktop, web or mobile), not by the CLI, an
// MCP tool or an Intelligence command.
const TagPaseoGUI = "Paseo GUI"

// PaseoHomeTags returns a Watcher.Tags that reads each workspace's creation receipt in the Paseo
// home dir (paseo.CreationKey).
func PaseoHomeTags(home string) func(workspaceID string) ([]string, error) {
	return func(id string) ([]string, error) {
		key, err := paseo.CreationKey(home, id)
		if err != nil {
			return nil, err
		}
		if paseo.CreatedInApp(key) {
			return []string{TagPaseoGUI}, nil
		}
		return nil, nil
	}
}

// tag looks up the tags of each workspace in ids that has none cached, outside the lock: a lookup
// reads files. A failed lookup is not cached, so the workspace's next update tries again.
func (w *Watcher) tag(ids []string) {
	if w.Tags == nil {
		return
	}
	w.mu.Lock()
	var todo []string
	for _, id := range ids {
		if _, ok := w.tags[id]; !ok {
			todo = append(todo, id)
		}
	}
	w.mu.Unlock()
	found := map[string][]string{}
	for _, id := range todo {
		if t, err := w.Tags(id); err == nil {
			found[id] = t
		}
	}
	w.mu.Lock()
	if w.tags == nil {
		w.tags = map[string][]string{}
	}
	for id, t := range found {
		w.tags[id] = t
	}
	w.mu.Unlock()
}
