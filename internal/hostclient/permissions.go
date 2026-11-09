package hostclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

const permissionRewrite = "Intelligence denied this permission prompt automatically because this agent is in a never-ask mode. Do the authorized step another way: use the Write/Edit tool for files instead of shell heredocs, split compound shell commands, and use git -C instead of cd && git. Write rm only with literal absolute paths (no glob, variable, substitution or ~, and no cd earlier in the same command). Do not retry the same blocked command or broaden your task. If no safe approach works, report the blocker."

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
	FirstSeenAt  time.Time `json:"firstSeenAt,omitzero"`
	DeniedAt     time.Time `json:"deniedAt,omitzero"`
	StuckAt      time.Time `json:"stuckAt,omitzero"`
	ClearedAt    time.Time `json:"clearedAt,omitzero"`
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

// PermissionSync keeps one audit row per prompt. Failed denials and uploads
// back off independently, doubling from one second to five minutes.
type PermissionSync struct {
	Send    func(context.Context, PermissionResolution) error
	Deny    func(context.Context, string, string) error
	Now     func() time.Time
	prompts map[string]*permissionPrompt
}

type permissionPrompt struct {
	record                 PermissionResolution
	dirty                  bool
	nextDeny, nextUpload   time.Time
	denyDelay, uploadDelay time.Duration
}

func retryDelay(previous time.Duration) time.Duration {
	if previous == 0 {
		return time.Second
	}
	return min(previous*2, 5*time.Minute)
}

func (s *PermissionSync) upload(ctx context.Context, p *permissionPrompt, now time.Time) error {
	if !p.dirty || now.Before(p.nextUpload) {
		return nil
	}
	if err := s.Send(ctx, p.record); err != nil {
		p.uploadDelay = retryDelay(p.uploadDelay)
		p.nextUpload = now.Add(p.uploadDelay)
		return err
	}
	p.dirty = false
	p.uploadDelay = 0
	p.nextUpload = time.Time{}
	return nil
}

func (s *PermissionSync) Sync(ctx context.Context, agents []paseo.Agent) error {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.prompts == nil {
		s.prompts = map[string]*permissionPrompt{}
	}
	now := s.Now()
	live := map[string]bool{}
	allowed := map[string]bool{}
	for _, a := range agents {
		if !a.ArchivedAt.IsZero() {
			continue
		}
		for _, request := range a.PendingPermissions {
			if request.ID == "" {
				continue
			}
			key := a.ID + "/" + request.ID
			live[key] = true
			if !neverAsk(a.Provider, a.CurrentModeID) {
				continue
			}
			allowed[key] = true
			if s.prompts[key] == nil {
				s.prompts[key] = &permissionPrompt{dirty: true, record: PermissionResolution{
					AgentID: a.ID, PermissionID: request.ID, Provider: a.Provider, Mode: a.CurrentModeID, Tool: request.Name,
					Status: "pending", Message: permissionRewrite, At: now, FirstSeenAt: now}}
			}
		}
	}
	var errs []error
	for key, p := range s.prompts {
		if err := s.upload(ctx, p, now); err != nil {
			errs = append(errs, err)
			continue
		}
		if p.dirty {
			continue
		} // an upload is waiting for its backoff
		r := &p.record
		if !live[key] {
			if r.Status != "cleared" {
				r.Status, r.ClearedAt, r.At = "cleared", now, now
				p.dirty = true
				if err := s.upload(ctx, p, now); err != nil {
					errs = append(errs, err)
				}
			}
			if !p.dirty {
				delete(s.prompts, key)
			}
			continue
		}
		if !allowed[key] {
			continue
		}
		since := r.FirstSeenAt
		if !r.DeniedAt.IsZero() {
			since = r.DeniedAt
		}
		if r.StuckAt.IsZero() && now.Sub(since) >= 20*time.Minute {
			r.Status, r.StuckAt, r.At = "stuck", now, now
			p.dirty = true
		}
		if r.DeniedAt.IsZero() && !now.Before(p.nextDeny) {
			err := s.Deny(ctx, r.AgentID, r.PermissionID)
			r.At = now
			if err != nil {
				r.Error = err.Error()
				if r.StuckAt.IsZero() {
					r.Status = "error"
				}
				p.denyDelay = retryDelay(p.denyDelay)
				p.nextDeny = now.Add(p.denyDelay)
				errs = append(errs, err)
			} else {
				r.DeniedAt, r.Error = now, ""
				if r.StuckAt.IsZero() {
					r.Status = "denied"
				}
			}
			p.dirty = true
		}
		if err := s.upload(ctx, p, now); err != nil {
			errs = append(errs, err)
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
		connected := b.Watcher.client != nil
		var agents []paseo.Agent
		for _, a := range b.Watcher.agents {
			agents = append(agents, a)
		}
		b.Watcher.mu.Unlock()
		if !connected {
			continue
		} // absence during a disconnect is not proof the prompt cleared
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Sync(call, agents)
		cancel()
		if err != nil && fmt.Sprint(err) != last && ctx.Err() == nil {
			b.Logf("permission watchdog: %v", err)
		}
		last = fmt.Sprint(err)
	}
}
