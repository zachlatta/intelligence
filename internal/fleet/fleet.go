// Package fleet stores the latest report from each dev box in Postgres (the devboxes table, read
// through the devbox_status view).
package fleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/devbox"
)

// Box is a dev box as the server last heard from it.
type Box struct {
	Report   devbox.Report `json:"report"`
	LastSeen time.Time     `json:"lastSeen"` // when the server received the report
	Online   bool          `json:"online"`   // it reported within the last minute
}

// Registry is the dev boxes' table.
type Registry struct{ p *pgxpool.Pool }

// New returns the registry on p, which must be migrated (db.Migrate).
func New(p *pgxpool.Pool) *Registry { return &Registry{p: p} }

// Record stores rep, received at at, as its box's latest report, with its agents.
func (r *Registry) Record(ctx context.Context, rep devbox.Report, at time.Time) error {
	ps := rep.Paseo
	providers, err := json.Marshal(nonNil(ps.Providers))
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO devboxes (name, client_version, collected_at, last_seen, paseo_error, daemon, connected,
			paseo_version, hostname, listen, pid, started_at, note, providers, providers_at, agents_error)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (name) DO UPDATE SET client_version = EXCLUDED.client_version, collected_at = EXCLUDED.collected_at,
			last_seen = EXCLUDED.last_seen, paseo_error = EXCLUDED.paseo_error, daemon = EXCLUDED.daemon,
			connected = EXCLUDED.connected, paseo_version = EXCLUDED.paseo_version, hostname = EXCLUDED.hostname,
			listen = EXCLUDED.listen, pid = EXCLUDED.pid, started_at = EXCLUDED.started_at, note = EXCLUDED.note,
			providers = EXCLUDED.providers, providers_at = EXCLUDED.providers_at, agents_error = EXCLUDED.agents_error`,
			rep.Name, rep.Version, nullTime(rep.CollectedAt), at, ps.Error, ps.Daemon, ps.Connected, ps.Version, ps.Hostname,
			ps.Listen, ps.PID, nullTime(ps.StartedAt), ps.Note, providers, nullTime(ps.ProvidersAt), ps.AgentsError)
		if err != nil {
			return err
		}
		// the report's list replaces the box's agents; a failed list leaves none rather than a stale one
		if _, err := tx.Exec(ctx, "DELETE FROM devbox_agents WHERE box = $1", rep.Name); err != nil {
			return err
		}
		rows := make([][]any, 0, len(ps.Agents))
		seen := map[string]bool{}
		for _, a := range ps.Agents {
			if a.ID == "" || seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			rows = append(rows, []any{rep.Name, a.ID, a.ShortID, a.Name, a.Provider, a.Thinking, a.Status, a.Cwd})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"devbox_agents"},
			[]string{"box", "id", "short_id", "name", "provider", "thinking", "status", "cwd"}, pgx.CopyFromRows(rows))
		return err
	})
}

// List returns every box, by name, each with its agents: running first, then by status and name.
func (r *Registry) List(ctx context.Context) ([]Box, error) {
	rows, err := r.p.Query(ctx, `SELECT name, client_version, collected_at, last_seen, online, paseo_error, daemon, connected,
		paseo_version, hostname, listen, pid, started_at, note, providers, providers_at, agents_error
		FROM devbox_status ORDER BY name`)
	if err != nil {
		return nil, err
	}
	boxes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Box, error) {
		var b Box
		var collected, started, providersAt *time.Time
		var providers []byte
		p := &b.Report.Paseo
		err := row.Scan(&b.Report.Name, &b.Report.Version, &collected, &b.LastSeen, &b.Online, &p.Error, &p.Daemon,
			&p.Connected, &p.Version, &p.Hostname, &p.Listen, &p.PID, &started, &p.Note, &providers, &providersAt,
			&p.AgentsError)
		if err != nil {
			return b, err
		}
		b.Report.CollectedAt, p.StartedAt, p.ProvidersAt = deref(collected), deref(started), deref(providersAt)
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
	rows, err = r.p.Query(ctx, `SELECT box, id, short_id, name, provider, thinking, status, cwd FROM devbox_agents
		ORDER BY box, status <> 'running', status, name, id`)
	if err != nil {
		return nil, err
	}
	agents := map[string][]devbox.Agent{}
	var box string
	var a devbox.Agent
	_, err = pgx.ForEachRow(rows, []any{&box, &a.ID, &a.ShortID, &a.Name, &a.Provider, &a.Thinking, &a.Status, &a.Cwd}, func() error {
		agents[box] = append(agents[box], a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range boxes {
		boxes[i].Report.Paseo.Agents = agents[boxes[i].Report.Name]
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

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
