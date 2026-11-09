package hostclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

const permissionRewrite = "Intelligence denied this permission prompt automatically because this agent is in a never-ask mode. Do the authorized step another way: use the Write/Edit tool for files instead of shell heredocs, split compound shell commands, and use git -C instead of cd && git. Do not retry the same blocked command or broaden your task. If no safe approach works, report the blocker."

// PermissionResolution is an audit of one prompt. pending is stored before the
// daemon is contacted; denied means the daemon acknowledged, error means retry.
type PermissionResolution struct {
	AgentID      string    `json:"agentId"`
	PermissionID string    `json:"permissionId"`
	Provider     string    `json:"provider"`
	Mode         string    `json:"mode"`
	Tool         string    `json:"tool"`
	Status       string    `json:"status"`
	Message      string    `json:"message"`
	Error        string    `json:"error,omitempty"`
	At           time.Time `json:"at"`
}

func neverAsk(provider, mode string) bool {
	switch provider {
	case "claude":
		return mode == "bypassPermissions"
	case "codex", "antigravity":
		return mode == "full-access"
	case "muse":
		return mode == "allowAll"
	}
	return false
}

// PermissionSync retries failed audits and denials. Only explicit provider/mode
// pairs qualify. A confirmed denial is never repeated while its prompt remains.
type PermissionSync struct {
	Send    func(context.Context, PermissionResolution) error
	Deny    func(context.Context, string, string) error
	done    map[string]bool
	pending map[string]PermissionResolution
}

func (s *PermissionSync) Sync(ctx context.Context, agents []paseo.Agent) error {
	if s.done == nil {
		s.done = map[string]bool{}
		s.pending = map[string]PermissionResolution{}
	}
	// Finish recording acknowledged responses even if the prompt/agent has gone.
	for key, r := range s.pending {
		if err := s.Send(ctx, r); err != nil {
			return err
		}
		delete(s.pending, key)
	}
	live := map[string]bool{}
	var errs []error
	for _, a := range agents {
		if !a.ArchivedAt.IsZero() || !neverAsk(a.Provider, a.CurrentModeID) {
			continue
		}
		for _, p := range a.PendingPermissions {
			if p.ID == "" {
				continue
			}
			key := a.ID + "/" + p.ID
			live[key] = true
			if s.done[key] {
				continue
			}
			r := PermissionResolution{AgentID: a.ID, PermissionID: p.ID, Provider: a.Provider, Mode: a.CurrentModeID, Tool: p.Name,
				Status: "pending", Message: permissionRewrite, At: time.Now()}
			if err := s.Send(ctx, r); err != nil {
				errs = append(errs, err)
				continue
			}
			err := s.Deny(ctx, a.ID, p.ID)
			r.At = time.Now()
			if err != nil {
				r.Status = "error"
				r.Error = err.Error()
				errs = append(errs, err)
			} else {
				r.Status = "denied"
				s.done[key] = true
			}
			s.pending[key] = r
			if err := s.Send(ctx, r); err != nil {
				errs = append(errs, err)
			} else {
				delete(s.pending, key)
			}
		}
	}
	for k := range s.done {
		if !live[k] {
			delete(s.done, k)
		}
	}
	return errors.Join(errs...)
}

func (b *Client) watchPermissions(ctx context.Context) {
	s := PermissionSync{Send: b.Link.Permission, Deny: func(ctx context.Context, agent, id string) error {
		b.Watcher.mu.Lock()
		c := b.Watcher.client
		b.Watcher.mu.Unlock()
		if c == nil {
			return errNoDaemon
		}
		return c.DenyPermission(ctx, agent, id, permissionRewrite)
	}}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		b.Watcher.mu.Lock()
		var agents []paseo.Agent
		for _, a := range b.Watcher.agents {
			agents = append(agents, a)
		}
		b.Watcher.mu.Unlock()
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Sync(call, agents)
		cancel()
		if err != nil && fmt.Sprint(err) != last && ctx.Err() == nil {
			b.Logf("permission watchdog: %v", err)
		}
		last = fmt.Sprint(err)
	}
}
