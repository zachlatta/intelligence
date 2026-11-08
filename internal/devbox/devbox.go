// Package devbox is the dev-box side of Intelligence: it reads the local Paseo daemon's state and
// reports it to the server. A dev box dials out to the server, so it needs no inbound port.
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

// PaseoStatus is what `paseo status` and `paseo ls` say about the box's daemon.
type PaseoStatus struct {
	Error     string     `json:"error,omitempty"`    // paseo status could not be run or read; the rest is empty
	Daemon    string     `json:"daemon"`             // localDaemon: running, stopped, ...
	Connected string     `json:"connected"`          // connectedDaemon: reachable, unreachable, ...
	Version   string     `json:"version"`            // daemonVersion
	Hostname  string     `json:"hostname"`           // as the daemon reports it
	Listen    string     `json:"listen"`             // the daemon's listen address
	PID       int        `json:"pid"`                // the daemon supervisor's pid
	StartedAt time.Time  `json:"startedAt,omitzero"` // when the daemon started
	Note      string     `json:"note,omitempty"`     // paseo's own note, such as a details timeout
	Providers []Provider `json:"providers,omitempty"`
	// ProvidersAt is when Providers was read. When paseo provider ls fails, the collector keeps the
	// last list it read, with its time.
	ProvidersAt time.Time `json:"providersAt,omitzero"`
	Agents      []Agent   `json:"agentList,omitempty"`   // every unarchived agent on the daemon, from paseo ls -g
	AgentsError string    `json:"agentsError,omitempty"` // paseo ls could not be run or read
}

// Agent is one Paseo agent on a dev box's daemon.
type Agent struct {
	ID       string `json:"id"`
	ShortID  string `json:"shortId"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // provider/model, such as claude/claude-opus-5-5
	Thinking string `json:"thinking"`
	Status   string `json:"status"` // running, idle, closed, error, ...
	Cwd      string `json:"cwd"`
}

// Provider is one agent provider and whether the daemon can run it (paseo provider ls).
type Provider struct {
	Name      string `json:"name"`
	Status    string `json:"status"`    // paseo's word: available, unavailable, error, ...
	Available bool   `json:"available"` // Status is "available"
}

// Runner runs a command and returns its stdout (exec.CommandContext(...).Output in production).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Collector reads the local Paseo daemon. It is not safe for concurrent use.
type Collector struct {
	Run Runner
	Now func() time.Time
	Bin string // the paseo executable; "" = "paseo" on PATH

	lastProviders   []Provider
	lastProvidersAt time.Time
}

// Paseo reads the daemon's status and agent counts. Failures are reported in the result, not returned.
func (c *Collector) Paseo(ctx context.Context) PaseoStatus {
	bin := c.Bin
	if bin == "" {
		bin = "paseo"
	}
	var raw struct {
		PID             int       `json:"pid"`
		StartedAt       time.Time `json:"startedAt"`
		Listen          string    `json:"listen"`
		Hostname        string    `json:"hostname"`
		LocalDaemon     string    `json:"localDaemon"`
		DaemonVersion   string    `json:"daemonVersion"`
		ConnectedDaemon string    `json:"connectedDaemon"`
		Note            string    `json:"note"`
	}
	out, err := c.Run(ctx, bin, "status", "--json")
	if err == nil {
		err = json.Unmarshal(out, &raw)
	}
	if err != nil {
		return PaseoStatus{Error: "paseo status: " + err.Error()}
	}
	s := PaseoStatus{Daemon: raw.LocalDaemon, Connected: raw.ConnectedDaemon, Version: raw.DaemonVersion,
		Hostname: raw.Hostname, Listen: raw.Listen, PID: raw.PID, StartedAt: raw.StartedAt, Note: raw.Note}
	var providers []struct {
		Provider string `json:"provider"`
		Status   string `json:"status"`
	}
	// paseo status leaves its provider details out when they time out; provider ls answers reliably
	if out, err := c.Run(ctx, bin, "provider", "ls", "--json"); err == nil && json.Unmarshal(out, &providers) == nil {
		c.lastProviders = make([]Provider, 0, len(providers))
		for _, p := range providers {
			c.lastProviders = append(c.lastProviders, Provider{Name: p.Provider, Status: p.Status, Available: p.Status == "available"})
		}
		c.lastProvidersAt = c.Now()
	}
	s.Providers, s.ProvidersAt = c.lastProviders, c.lastProvidersAt

	// -g: every directory's agents, not only those under this process's working directory
	out, err = c.Run(ctx, bin, "ls", "-g", "--json")
	if err == nil {
		err = json.Unmarshal(out, &s.Agents)
	}
	if err != nil {
		s.Agents, s.AgentsError = nil, "paseo ls: "+err.Error()
	}
	return s
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
