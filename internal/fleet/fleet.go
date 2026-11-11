// Package fleet stores each host's latest report in Postgres as hosts > workspaces > agents.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Host is a host as the server last heard from it, with its workspaces and their agents.
// Report.Paseo carries the daemon's status; its Workspaces and Agents are empty here.
type Host struct {
	Report     hostclient.Report `json:"report"`
	LastSeen   time.Time         `json:"lastSeen"` // when the server last heard from it
	Online     bool              `json:"online"`   // it holds a stream to the server
	Workspaces []WorkspaceAgents `json:"workspaces"`
}

// WorkspaceAgents is a workspace and the agents in it.
type WorkspaceAgents struct {
	hostclient.Workspace
	Listed bool        `json:"listed"` // false: Paseo did not list it, but an agent is in it
	Agents []AgentView `json:"agents"`
}

// AgentView is an agent and how much of its transcript is synced.
type AgentView struct {
	PermissionStuck bool `json:"permissionStuck"`
	hostclient.Agent
	TranscriptSynced  bool `json:"transcriptSynced"`
	TranscriptEntries int  `json:"transcriptEntries"`
	Archived          bool `json:"archived"`
}

// Registry is the hosts, workspaces and agents tables.
type Registry struct{ p *pgxpool.Pool }

// New returns the registry on p, which must be migrated (db.Migrate).
func New(p *pgxpool.Pool) *Registry { return &Registry{p: p} }

// Record stores rep, received at at, as its host's latest state: it replaces the host's
// workspaces and agents.
func (r *Registry) Record(ctx context.Context, rep hostclient.Report, at time.Time) error {
	return r.record(ctx, rep, at, "")
}

// RecordStream accepts reports only from the current stream. The host row lock
// keeps a replacement from inheriting a report's retry capability.
func (r *Registry) RecordStream(ctx context.Context, rep hostclient.Report, at time.Time, stream string) error {
	return r.record(ctx, rep, at, stream)
}

func (r *Registry) record(ctx context.Context, rep hostclient.Report, at time.Time, stream string) error {
	ps := rep.Paseo
	providers, err := json.Marshal(nonNil(ps.Providers))
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		if stream != "" {
			tag, err := tx.Exec(ctx, "UPDATE hosts SET start_retry_keys = $3 WHERE name = $1 AND stream_id = $2", rep.Name, stream, rep.StartRetryKeys)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return errors.New("host stream was replaced")
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (name, client_version, collected_at, last_seen, connected, paseo_error,
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
		if err := CaptureWorkflowAgents(ctx, tx, rep.Name, "", at); err != nil {
			return err
		}
		projects, err := json.Marshal(nonNil(ps.Projects))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE hosts SET projects = $2, report_received_at = $3 WHERE name = $1", rep.Name, projects, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM workspaces WHERE host = $1", rep.Name); err != nil { // and their agents
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
				w.Additions, w.Deletions, nullTime(w.CreatedAt), nullTime(w.ActivityAt), nullTime(w.LastUserMessageAt), w.ProjectRoot, nonNil(w.Tags)})
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
				wrows = append(wrows, []any{rep.Name, a.WorkspaceID, false, "", "", "", a.Cwd, "", "", false, 0, 0, nil, nil, nil, "", []string{}})
			}
			arows = append(arows, []any{rep.Name, a.ID, a.WorkspaceID, a.ParentID, a.Title, a.Provider, a.Model, a.Thinking,
				a.Status, a.Cwd, a.RequiresAttention, a.AttentionReason, nullTime(a.CreatedAt), nullTime(a.UpdatedAt),
				nullTime(a.LastUserMessageAt)})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"workspaces"}, []string{"host", "id", "listed", "name", "kind", "status",
			"directory", "project", "branch", "dirty", "additions", "deletions", "created_at", "activity_at",
			"last_user_message_at", "project_root", "tags"}, pgx.CopyFromRows(wrows)); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"agents"}, []string{"host", "id", "workspace_id", "parent_id", "title",
			"provider", "model", "thinking", "status", "cwd", "requires_attention", "attention_reason", "created_at",
			"updated_at", "last_user_message_at"}, pgx.CopyFromRows(arows)); err != nil {
			return err
		}
		// Preserve departing agents before replacement, and capture newly reported
		// workers now so their resource samples reduce reservations immediately.
		if err := CaptureWorkflowAgents(ctx, tx, rep.Name, "", at); err != nil {
			return err
		}
		if !ps.Connected {
			return nil // a host that lost its daemon keeps its transcripts and diffs until it can say what is gone
		}
		// A connected daemon's complete agent list confirms archival. An outage leaves
		// these flags untouched; seeing an agent again clears its prior archived state.
		if _, err := tx.Exec(ctx, `UPDATE workflow_run_agents h SET archived=true
            WHERE h.host=$1 AND NOT h.archived
                AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.host=h.host AND a.id=h.agent_id)`, rep.Name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_run_agents h SET archived=false FROM agents a
            WHERE a.host=$1 AND h.host=a.host AND h.agent_id=a.id AND h.archived`, rep.Name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_diffs d WHERE d.host = $1
			AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.host = d.host AND w.id = d.workspace_id AND w.listed)
            AND NOT EXISTS (SELECT 1 FROM workflow_run_agents h WHERE h.host = d.host AND h.workspace->>'id' = d.workspace_id)`, rep.Name); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM transcripts t WHERE t.host = $1
			AND NOT EXISTS (SELECT 1 FROM recorded_agents a WHERE a.host = t.host AND a.id = t.agent_id)`, rep.Name)
		return err
	})
}

// List returns every host by name. Its workspaces come with running agents first, then with any
// agents, then the most recently active; agents are running first, then newest.
func (r *Registry) List(ctx context.Context) ([]Host, error) {
	return r.list(ctx, false)
}

// ListWithHistory includes a run's archived agents for transcript navigation. The live fleet
// continues to use List, so archived agents do not appear active or offer workspaces to reuse.
func (r *Registry) ListWithHistory(ctx context.Context) ([]Host, error) {
	return r.list(ctx, true)
}

func (r *Registry) list(ctx context.Context, history bool) ([]Host, error) {
	agentTable, workspaceTable := "agents", "workspace_status"
	if history {
		agentTable, workspaceTable = "recorded_agents", "recorded_workspace_status"
	}
	rows, err := r.p.Query(ctx, `SELECT name, client_version, collected_at, last_seen, online, connected, paseo_error,
		status_error, paseo_version, hostname, listen, pid, started_at, providers, providers_at,
		(SELECT projects FROM hosts WHERE hosts.name = host_status.name),
		(SELECT start_retry_keys FROM hosts WHERE hosts.name = host_status.name)
		FROM host_status ORDER BY name`)
	if err != nil {
		return nil, err
	}
	hosts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Host, error) {
		var b Host
		var collected, started, providersAt *time.Time
		var providers, projects []byte
		p := &b.Report.Paseo
		if err := row.Scan(&b.Report.Name, &b.Report.Version, &collected, &b.LastSeen, &b.Online, &p.Connected, &p.Error,
			&p.StatusError, &p.Version, &p.Hostname, &p.Listen, &p.PID, &started, &providers, &providersAt, &projects, &b.Report.StartRetryKeys); err != nil {
			return b, err
		}
		b.Report.CollectedAt, p.StartedAt, p.ProvidersAt = deref(collected), deref(started), deref(providersAt)
		b.LastSeen = b.LastSeen.UTC()
		if err := json.Unmarshal(providers, &p.Providers); err != nil {
			return b, err
		}
		if err := json.Unmarshal(projects, &p.Projects); err != nil {
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
	for i := range hosts {
		index[hosts[i].Report.Name] = i
	}

	type key struct{ host, id string }
	agents := map[key][]AgentView{}
	rows, err = r.p.Query(ctx, `SELECT a.host, a.workspace_id, a.id, a.parent_id, a.title, a.provider, a.model, a.thinking,
		a.status, a.cwd, a.requires_attention, a.attention_reason, a.created_at, a.updated_at, a.last_user_message_at,
		EXISTS (SELECT 1 FROM permission_resolutions pr WHERE pr.host=a.host AND pr.agent_id=a.id AND pr.status='stuck' AND pr.cleared_at IS NULL),
		t.agent_id IS NOT NULL, (NOT EXISTS (SELECT 1 FROM agents live WHERE live.host = a.host AND live.id = a.id)
            AND EXISTS (SELECT 1 FROM archived_workflow_agents h WHERE h.host=a.host AND h.agent_id=a.id)),
		(SELECT count(*) FROM transcript_entries e WHERE e.host = a.host AND e.agent_id = a.id)::int
		FROM `+agentTable+` a LEFT JOIN transcripts t ON t.host = a.host AND t.agent_id = a.id
		ORDER BY a.host, a.status <> 'running', a.created_at DESC NULLS LAST, a.id`)
	if err != nil {
		return nil, err
	}
	var host string
	var a AgentView
	var created, updated, lastMsg *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&host, &a.WorkspaceID, &a.ID, &a.ParentID, &a.Title, &a.Provider, &a.Model,
		&a.Thinking, &a.Status, &a.Cwd, &a.RequiresAttention, &a.AttentionReason, &created, &updated, &lastMsg,
		&a.PermissionStuck, &a.TranscriptSynced, &a.Archived, &a.TranscriptEntries}, func() error {
		a.CreatedAt, a.UpdatedAt, a.LastUserMessageAt = deref(created), deref(updated), deref(lastMsg)
		k := key{host, a.WorkspaceID}
		agents[k] = append(agents[k], a)
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = r.p.Query(ctx, `SELECT host, id, listed, name, kind, status, directory, project, branch, dirty, additions,
		deletions, created_at, activity_at, last_user_message_at, project_root, tags
		FROM `+workspaceTable+`
		ORDER BY host, running_agents > 0 DESC, agents > 0 DESC,
		         greatest(activity_at, last_user_message_at, created_at) DESC NULLS LAST, name, id`)
	if err != nil {
		return nil, err
	}
	var w WorkspaceAgents
	var wCreated, wActivity, wLastMsg *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&host, &w.ID, &w.Listed, &w.Name, &w.Kind, &w.Status, &w.Directory, &w.Project,
		&w.Branch, &w.Dirty, &w.Additions, &w.Deletions, &wCreated, &wActivity, &wLastMsg, &w.ProjectRoot, &w.Tags}, func() error {
		w.CreatedAt, w.ActivityAt, w.LastUserMessageAt = deref(wCreated), deref(wActivity), deref(wLastMsg)
		if len(w.Tags) == 0 {
			w.Tags = nil
		}
		w.Agents = treeOrder(agents[key{host, w.ID}])
		if i, ok := index[host]; ok {
			hosts[i].Workspaces = append(hosts[i].Workspaces, w)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return hosts, nil
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

// treeOrder puts each sub-agent right after its parent, keeping the given order among siblings
// and among top-level agents. A sub-agent whose parent is not in the list counts as top-level.
func treeOrder(agents []AgentView) []AgentView {
	in := make(map[string]bool, len(agents))
	for _, a := range agents {
		in[a.ID] = true
	}
	children := map[string][]int{}
	var roots []int
	for i, a := range agents {
		if a.ParentID != "" && a.ParentID != a.ID && in[a.ParentID] {
			children[a.ParentID] = append(children[a.ParentID], i)
		} else {
			roots = append(roots, i)
		}
	}
	out := make([]AgentView, 0, len(agents))
	done := make([]bool, len(agents))
	var walk func(i int)
	walk = func(i int) {
		if done[i] {
			return
		}
		done[i] = true
		out = append(out, agents[i])
		for _, c := range children[agents[i].ID] {
			walk(c)
		}
	}
	for _, i := range roots {
		walk(i)
	}
	for i := range agents { // a parent loop has no root: keep its agents rather than drop them
		walk(i)
	}
	return out
}
