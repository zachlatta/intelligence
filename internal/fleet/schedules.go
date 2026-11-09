package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

// ErrNoSchedule: no such schedule on the host (or not the account's).
var ErrNoSchedule = errors.New("no such schedule")

// ScheduleView is a schedule as the server last heard of it, with its runs counted and its newest.
type ScheduleView struct {
	hostclient.Schedule
	Host       string   `json:"host"`
	RunCount   int      `json:"runCount"`
	FailedRuns int      `json:"failedRuns"`
	Running    bool     `json:"running"`
	LastRun    *RunView `json:"lastRun,omitempty"`
}

// RunView is one run, and whether its agent is still on the host (so its transcript can be read).
type RunView struct {
	hostclient.ScheduleRun
	AgentListed bool `json:"agentListed"`
}

// RecordSchedules stores a host's schedules, received at at: they replace the host's schedules,
// and the runs that came with them are added to (or update) the ones kept. A report with an
// Error keeps the schedules the server has.
func (r *Registry) RecordSchedules(ctx context.Context, host string, rep hostclient.ScheduleReport, at time.Time) error {
	collected := rep.CollectedAt
	if collected.IsZero() {
		collected = at
	}
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO hosts (name, last_seen, schedules_at, schedules_error) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET schedules_at = EXCLUDED.schedules_at, schedules_error = EXCLUDED.schedules_error`,
			host, at, collected, rep.Error); err != nil {
			return err
		}
		if rep.Error != "" {
			return nil
		}
		ids := make([]string, 0, len(rep.Schedules))
		for _, s := range rep.Schedules {
			if s.ID == "" {
				continue
			}
			ids = append(ids, s.ID)
			var every *int64
			if s.EveryMs > 0 {
				every = &s.EveryMs
			}
			var maxRuns *int
			if s.MaxRuns > 0 {
				maxRuns = &s.MaxRuns
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schedules (host, id, name, prompt, status, every_ms, cron, timezone, target, agent_id,
				provider, cwd, model, mode, thinking, isolation, archive_on_finish, title, max_runs, created_at, updated_at, next_run_at,
				last_run_at, paused_at, expires_at, received_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
				ON CONFLICT (host, id) DO UPDATE SET name = EXCLUDED.name, prompt = EXCLUDED.prompt, status = EXCLUDED.status,
				every_ms = EXCLUDED.every_ms, cron = EXCLUDED.cron, timezone = EXCLUDED.timezone, target = EXCLUDED.target,
				agent_id = EXCLUDED.agent_id, provider = EXCLUDED.provider, cwd = EXCLUDED.cwd, model = EXCLUDED.model,
				mode = EXCLUDED.mode, thinking = EXCLUDED.thinking, isolation = EXCLUDED.isolation,
				archive_on_finish = EXCLUDED.archive_on_finish, title = EXCLUDED.title, max_runs = EXCLUDED.max_runs,
				created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at, next_run_at = EXCLUDED.next_run_at,
				last_run_at = EXCLUDED.last_run_at, paused_at = EXCLUDED.paused_at, expires_at = EXCLUDED.expires_at,
				received_at = EXCLUDED.received_at`,
				host, s.ID, s.Name, s.Prompt, s.Status, every, s.Cron, s.Timezone, s.Target, s.AgentID, s.Provider, s.Cwd, s.Model,
				s.Mode, s.Thinking, s.Isolation, s.ArchiveOnFinish, s.Title, maxRuns, nullTime(s.CreatedAt), nullTime(s.UpdatedAt),
				nullTime(s.NextRunAt), nullTime(s.LastRunAt), nullTime(s.PausedAt), nullTime(s.ExpiresAt), at); err != nil {
				return err
			}
			if !s.RunsIncluded {
				continue
			}
			for _, run := range s.Runs {
				if run.ID == "" {
					continue
				}
				if _, err := tx.Exec(ctx, `INSERT INTO schedule_runs (host, schedule_id, id, status, scheduled_for, started_at, ended_at,
					agent_id, workspace_id, output, error, received_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
					ON CONFLICT (host, schedule_id, id) DO UPDATE SET status = EXCLUDED.status, scheduled_for = EXCLUDED.scheduled_for,
					started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at, agent_id = EXCLUDED.agent_id,
					workspace_id = EXCLUDED.workspace_id, output = EXCLUDED.output, error = EXCLUDED.error, received_at = EXCLUDED.received_at`,
					host, s.ID, run.ID, run.Status, nullTime(run.ScheduledFor), nullTime(run.StartedAt), nullTime(run.EndedAt), run.AgentID,
					run.WorkspaceID, run.Output, run.Error, at); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(ctx, "DELETE FROM schedules WHERE host = $1 AND NOT id = ANY($2)", host, ids) // and their runs
		return err
	})
}

const scheduleColumns = `host, id, name, prompt, status, coalesce(every_ms, 0), cron, timezone, target, agent_id, provider, cwd, model,
	mode, thinking, isolation, archive_on_finish, title, coalesce(max_runs, 0), created_at, updated_at, next_run_at, last_run_at,
	paused_at, expires_at, runs, failed_runs, running, last_run_id, last_run_status, last_run_started_at, last_run_agent_id, last_run_error`

func scanSchedule(row pgx.Row) (ScheduleView, error) {
	var s ScheduleView
	var created, updated, next, last, paused, expires, lastStarted *time.Time
	var lastID, lastStatus, lastAgent, lastError *string
	err := row.Scan(&s.Host, &s.ID, &s.Name, &s.Prompt, &s.Status, &s.EveryMs, &s.Cron, &s.Timezone, &s.Target, &s.AgentID,
		&s.Provider, &s.Cwd, &s.Model, &s.Mode, &s.Thinking, &s.Isolation, &s.ArchiveOnFinish, &s.Title, &s.MaxRuns, &created,
		&updated, &next, &last, &paused, &expires, &s.RunCount, &s.FailedRuns, &s.Running, &lastID, &lastStatus, &lastStarted, &lastAgent,
		&lastError)
	s.CreatedAt, s.UpdatedAt, s.NextRunAt, s.LastRunAt, s.PausedAt, s.ExpiresAt = deref(created), deref(updated), deref(next),
		deref(last), deref(paused), deref(expires)
	if lastStatus != nil {
		s.LastRun = &RunView{ScheduleRun: hostclient.ScheduleRun{ID: *lastID, Status: *lastStatus, StartedAt: deref(lastStarted), AgentID: *lastAgent,
			Error: *lastError}}
	}
	return s, err
}

// Schedules returns every schedule, by host and then name.
func (r *Registry) Schedules(ctx context.Context) ([]ScheduleView, error) {
	rows, err := r.p.Query(ctx, `SELECT `+scheduleColumns+` FROM schedule_status ORDER BY host, name = '', lower(name), id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScheduleView, error) { return scanSchedule(row) })
}

// Schedule returns one of a host's schedules and its runs, newest first.
func (r *Registry) Schedule(ctx context.Context, host, id string) (ScheduleView, []RunView, error) {
	s, err := scanSchedule(r.p.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM schedule_status WHERE host = $1 AND id = $2`, host, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil, ErrNoSchedule
	}
	if err != nil {
		return s, nil, err
	}
	rows, err := r.p.Query(ctx, `SELECT r.id, r.status, r.scheduled_for, r.started_at, r.ended_at, r.agent_id, r.workspace_id, r.output,
		r.error, EXISTS (SELECT 1 FROM agents a WHERE a.host = r.host AND a.id = r.agent_id)
		FROM schedule_runs r WHERE r.host = $1 AND r.schedule_id = $2 ORDER BY r.started_at DESC NULLS LAST, r.id DESC`, host, id)
	if err != nil {
		return s, nil, err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunView, error) {
		var v RunView
		var sched, started, ended *time.Time
		err := row.Scan(&v.ID, &v.Status, &sched, &started, &ended, &v.AgentID, &v.WorkspaceID, &v.Output, &v.Error, &v.AgentListed)
		v.ScheduledFor, v.StartedAt, v.EndedAt = deref(sched), deref(started), deref(ended)
		return v, err
	})
	return s, runs, err
}

// ScheduleProblems is why each host that could not list its schedules could not, by host name.
func (r *Registry) ScheduleProblems(ctx context.Context) (map[string]string, error) {
	rows, err := r.p.Query(ctx, "SELECT name, schedules_error FROM hosts WHERE schedules_error <> ''")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var host, msg string
	_, err = pgx.ForEachRow(rows, []any{&host, &msg}, func() error { out[host] = msg; return nil })
	return out, err
}
