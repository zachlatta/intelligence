// Package devbox is the dev-box side of Intelligence: it watches the local Paseo daemon over its
// own protocol (Watcher) and reports it to the server (SendReport). A dev box dials out to the
// server, so it needs no inbound port.
package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ReportPath is where a dev box POSTs its reports.
const ReportPath = "/api/devboxes/report"

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

// SendReport POSTs r to the server at base with the dev-box token. It returns the server's version
// (VersionHeader), also when the server refuses the report, so a box can update to a fixed server.
func SendReport(ctx context.Context, client *http.Client, base, token string, r Report) (string, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+ReportPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	v := resp.Header.Get(VersionHeader)
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return v, fmt.Errorf("report refused: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return v, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether s can name a dev box: lowercase letters, digits, '.', '_' and '-', at most 64.
func ValidName(s string) bool { return nameRe.MatchString(s) }
