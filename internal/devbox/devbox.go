// Package devbox is the dev-box side of Intelligence: it watches the local Paseo daemon over its
// own protocol (Watcher), holds one stream to the server (Link) for its reports, transcripts and
// the server's commands (Runner), and keeps itself on the server's version (Updater). A dev box
// dials out to the server, so it needs no inbound port.
package devbox

import (
	"regexp"
	"time"
)

// Report is one dev box's state at one moment.
type Report struct {
	Name        string      `json:"name"`        // the box's name, unique in the fleet (ValidName)
	CollectedAt time.Time   `json:"collectedAt"` // when the box read it
	Version     string      `json:"version"`     // the client binary's commit
	Paseo       PaseoStatus `json:"paseo"`
}

// PaseoStatus is the box's Paseo daemon: its status, workspaces and agents.
type PaseoStatus struct {
	Connected   bool        `json:"connected"`             // the box holds a connection to its daemon
	Error       string      `json:"error,omitempty"`       // why it is not connected; workspaces and agents are then empty
	StatusError string      `json:"statusError,omitempty"` // the last status read failed; the fields below are from the one before
	Version     string      `json:"version"`
	Hostname    string      `json:"hostname"`
	Listen      string      `json:"listen"`
	PID         int         `json:"pid"`
	StartedAt   time.Time   `json:"startedAt,omitzero"`
	Providers   []Provider  `json:"providers,omitempty"`
	ProvidersAt time.Time   `json:"providersAt,omitzero"` // when the status (and Providers) was read
	Workspaces  []Workspace `json:"workspaces,omitempty"`
	Agents      []Agent     `json:"agents,omitempty"` // every unarchived agent
}

// Provider is one agent provider and whether the daemon can run it.
type Provider struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

// Workspace is a directory, local checkout or git worktree on the box that agents run in.
type Workspace struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Kind              string    `json:"kind"`   // directory, local_checkout, worktree
	Status            string    `json:"status"` // running, attention, done
	Directory         string    `json:"directory"`
	Project           string    `json:"project"`
	Branch            string    `json:"branch,omitempty"`
	Dirty             bool      `json:"dirty"`
	Additions         int       `json:"additions"`
	Deletions         int       `json:"deletions"`
	CreatedAt         time.Time `json:"createdAt,omitzero"`
	ActivityAt        time.Time `json:"activityAt,omitzero"`
	LastUserMessageAt time.Time `json:"lastUserMessageAt,omitzero"`
}

// Agent is one Paseo agent, in one workspace.
type Agent struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"workspaceId"`
	ParentID          string    `json:"parentId,omitempty"` // the agent that started it, for sub-agents
	Title             string    `json:"title"`
	Provider          string    `json:"provider"` // claude, codex, ...
	Model             string    `json:"model"`
	Thinking          string    `json:"thinking"`
	Status            string    `json:"status"` // running, idle, closed, error, ...
	Cwd               string    `json:"cwd"`
	RequiresAttention bool      `json:"requiresAttention"`
	AttentionReason   string    `json:"attentionReason,omitempty"`
	CreatedAt         time.Time `json:"createdAt,omitzero"`
	UpdatedAt         time.Time `json:"updatedAt,omitzero"`
	LastUserMessageAt time.Time `json:"lastUserMessageAt,omitzero"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether s can name a dev box: lowercase letters, digits, '.', '_' and '-', at most 64.
func ValidName(s string) bool { return nameRe.MatchString(s) }
