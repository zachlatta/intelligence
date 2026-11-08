// Package fleet stores each dev box's latest report in Postgres as devboxes > workspaces > agents.
package fleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/devbox"
)

// Box is a dev box as the server last heard from it, with its workspaces and their agents.
// Report.Paseo carries the daemon's status; its Workspaces and Agents are empty here.
type Box struct {
	Report     devbox.Report     `json:"report"`
	LastSeen   time.Time         `json:"lastSeen"` // when the server received the report
	Online     bool              `json:"online"`   // it reported within the last minute
	Workspaces []WorkspaceAgents `json:"workspaces"`
}

// WorkspaceAgents is a workspace and the agents in it.
type WorkspaceAgents struct {
	devbox.Workspace
	Listed bool        `json:"listed"` // false: Paseo did not list it, but an agent is in it
	Agents []AgentView `json:"agents"`
}

// AgentView is an agent and how much of its transcript is synced.
type AgentView struct {
	devbox.Agent
	TranscriptSynced  bool `json:"transcriptSynced"`
	TranscriptEntries int  `json:"transcriptEntries"`
}

// Registry is the devboxes, workspaces and agents tables.
type Registry struct{ p *pgxpool.Pool }

// New returns the registry on p, which must be migrated (db.Migrate).
func New(p *pgxpool.Pool) *Registry { return &Registry{p: p} }

// Record stores rep, received at at, as its box's latest state: it replaces the box's
// workspaces and agents.
func (r *Registry) Record(ctx context.Context, rep devbox.Report, at time.Time) error {
	ps := rep.Paseo
	providers, err := json.Marshal(nonNil(ps.Providers))
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO devboxes (name, client_version, collected_at, last_seen, connected, paseo_error,
			status_error, paseo_version, hostname, listen, pid, started_at, providers, providers_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (name) DO UPDATE SET client_version = EXCLUDED.client_version, collected_at = EXCLUDED.collected_at,
			last_seen = EXCLUDED.last_seen, connected = EXCLUDED.connected, paseo_error = EXCLUDED.paseo_error,
			status_error = EXCLUDED.status_error, paseo_version = EXCLUDED.paseo_version, hostname = EXCLUDED.hostname,
			listen = EXCLUDED.listen, pid = EXCLUDED.pid, started_at = EXCLUDED.started_at, providers = EXCLUDED.providers,
			providers_at = EXCLUDED.providers_at`,
			rep.Name, rep.Version, nullTime(rep.CollectedAt), at, ps.Connected, ps.Error, ps.StatusError, ps.Version,
			ps.Hostname, ps.Listen, ps.PID, nullTime(ps.StartedAt), providers, nullTime(ps.ProvidersAt))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM workspaces WHERE box = $1", rep.Name); err != nil { // and their agents
			return err
		}
		listed := map[string]bool{}
		var wrows [][]any
		for _, w := range ps.Workspaces {
			if w.ID == "" || listed[w.ID] {
				continue
			}
			listed[w.ID] = true
			wrows = append(wrows, []any{rep.Name, w.ID, true, w.Name, w.Kind, w.Status, w.Directory, w.Project, w.Branch, w.Dirty,
				w.Additions, w.Deletions, nullTime(w.CreatedAt), nullTime(w.ActivityAt), nullTime(w.LastUserMessageAt)})
		}
		seen := map[string]bool{}
		var arows [][]any
		for _, a := range ps.Agents {
			if a.ID == "" || a.WorkspaceID == "" || seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			if !listed[a.WorkspaceID] {
				listed[a.WorkspaceID] = true // keep the agent: its workspace is archived or in the background
				wrows = append(wrows, []any{rep.Name, a.WorkspaceID, false, "", "", "", a.Cwd, "", "", false, 0, 0, nil, nil, nil})
			}
			arows = append(arows, []any{rep.Name, a.ID, a.WorkspaceID, a.ParentID, a.Title, a.Provider, a.Model, a.Thinking,
				a.Status, a.Cwd, a.RequiresAttention, a.AttentionReason, nullTime(a.CreatedAt), nullTime(a.UpdatedAt),
				nullTime(a.LastUserMessageAt)})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"workspaces"}, []string{"box", "id", "listed", "name", "kind", "status",
			"directory", "project", "branch", "dirty", "additions", "deletions", "created_at", "activity_at",
			"last_user_message_at"}, pgx.CopyFromRows(wrows)); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"agents"}, []string{"box", "id", "workspace_id", "parent_id", "title",
			"provider", "model", "thinking", "status", "cwd", "requires_attention", "attention_reason", "created_at",
			"updated_at", "last_user_message_at"}, pgx.CopyFromRows(arows)); err != nil {
			return err
		}
		if !ps.Connected {
			return nil // a box that lost its daemon keeps its transcripts until it can say which agents are gone
		}
		_, err = tx.Exec(ctx, `DELETE FROM transcripts t WHERE t.box = $1
			AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.box = t.box AND a.id = t.agent_id)`, rep.Name)
		return err
	})
}

// List returns every box by name. Its workspaces come with running agents first, then with any
// agents, then the most recently active; agents are running first, then newest.
func (r *Registry) List(ctx context.Context) ([]Box, error) {
	rows, err := r.p.Query(ctx, `SELECT name, client_version, collected_at, last_seen, online, connected, paseo_error,
		status_error, paseo_version, hostname, listen, pid, started_at, providers, providers_at
		FROM devbox_status ORDER BY name`)
	if err != nil {
		return nil, err
	}
	boxes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Box, error) {
		var b Box
		var collected, started, providersAt *time.Time
		var providers []byte
		p := &b.Report.Paseo
		if err := row.Scan(&b.Report.Name, &b.Report.Version, &collected, &b.LastSeen, &b.Online, &p.Connected, &p.Error,
			&p.StatusError, &p.Version, &p.Hostname, &p.Listen, &p.PID, &started, &providers, &providersAt); err != nil {
			return b, err
		}
		b.Report.CollectedAt, p.StartedAt, p.ProvidersAt = deref(collected), deref(started), deref(providersAt)
		b.LastSeen = b.LastSeen.UTC()
		if err := json.Unmarshal(providers, &p.Providers); err != nil {
			return b, err
		}
		if len(p.Providers) == 0 {
			p.Providers = nil
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for i := range boxes {
		index[boxes[i].Report.Name] = i
	}

	type key struct{ box, id string }
	agents := map[key][]AgentView{}
	rows, err = r.p.Query(ctx, `SELECT a.box, a.workspace_id, a.id, a.parent_id, a.title, a.provider, a.model, a.thinking,
		a.status, a.cwd, a.requires_attention, a.attention_reason, a.created_at, a.updated_at, a.last_user_message_at,
		t.agent_id IS NOT NULL,
		(SELECT count(*) FROM transcript_entries e WHERE e.box = a.box AND e.agent_id = a.id)::int
		FROM agents a LEFT JOIN transcripts t ON t.box = a.box AND t.agent_id = a.id
		ORDER BY a.box, a.status <> 'running', a.created_at DESC NULLS LAST, a.id`)
	if err != nil {
		return nil, err
	}
	var box string
	var a AgentView
	var created, updated, lastMsg *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&box, &a.WorkspaceID, &a.ID, &a.ParentID, &a.Title, &a.Provider, &a.Model,
		&a.Thinking, &a.Status, &a.Cwd, &a.RequiresAttention, &a.AttentionReason, &created, &updated, &lastMsg,
		&a.TranscriptSynced, &a.TranscriptEntries}, func() error {
		a.CreatedAt, a.UpdatedAt, a.LastUserMessageAt = deref(created), deref(updated), deref(lastMsg)
		k := key{box, a.WorkspaceID}
		agents[k] = append(agents[k], a)
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = r.p.Query(ctx, `SELECT box, id, listed, name, kind, status, directory, project, branch, dirty, additions,
		deletions, created_at, activity_at, last_user_message_at
		FROM workspace_status
		ORDER BY box, running_agents > 0 DESC, agents > 0 DESC,
		         greatest(activity_at, last_user_message_at, created_at) DESC NULLS LAST, name, id`)
	if err != nil {
		return nil, err
	}
	var w WorkspaceAgents
	var wCreated, wActivity, wLastMsg *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&box, &w.ID, &w.Listed, &w.Name, &w.Kind, &w.Status, &w.Directory, &w.Project,
		&w.Branch, &w.Dirty, &w.Additions, &w.Deletions, &wCreated, &wActivity, &wLastMsg}, func() error {
		w.CreatedAt, w.ActivityAt, w.LastUserMessageAt = deref(wCreated), deref(wActivity), deref(wLastMsg)
		w.Agents = agents[key{box, w.ID}]
		if i, ok := index[box]; ok {
			boxes[i].Workspaces = append(boxes[i].Workspaces, w)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return boxes, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// deref is a nullable timestamp from the database, in UTC.
func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
