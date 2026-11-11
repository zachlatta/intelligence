package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdmitHost retries durable queued runs when a host reports resources. The transaction lock
// serializes admissions across server instances; SKIP LOCKED avoids moves already in progress.
func (e *Engine) AdmitHost(ctx context.Context, host string) error {
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error { return e.admitHost(ctx, tx, host) })
}

func (e *Engine) admitHost(ctx context.Context, tx pgx.Tx, host string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 29))`, host); err != nil {
		return err
	}
	var total, available int64
	var minute time.Time
	err := tx.QueryRow(ctx, `SELECT mem_total, mem_available_min, minute FROM host_minutes
		WHERE host = $1 ORDER BY minute DESC LIMIT 1`, host).Scan(&total, &available, &minute)
	// Older hosts without resource telemetry retain their existing behavior. Once telemetry
	// exists, stale/unknown memory blocks admission until a useful sample arrives.
	legacy := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !legacy {
		return err
	}
	if !legacy && (total <= 0 || minute.Before(e.Now().Add(-3*time.Minute)) || minute.After(e.Now().Add(time.Minute))) {
		return nil
	}
	if !legacy {
		available -= max(int64(1<<30), total/10)
		// Free RAM already excludes resident agents. Reserve only the unobserved portion
		// of each admitted run's estimate, including commands not yet delivered.
		var reserved int64
		err := tx.QueryRow(ctx, `SELECT COALESCE(sum(greatest(0, r.estimated_memory_bytes - COALESCE((
			SELECT sum(m.mem_max) FROM workflow_run_agents h JOIN agent_minutes m
			ON m.host = h.host AND m.agent_id = h.agent_id AND m.minute = $2 WHERE h.run_id = r.id
		), 0))), 0)::bigint FROM workflow_runs r
		WHERE r.host = $1 AND r.status IN ('starting', 'running', 'waiting')`, host, minute).Scan(&reserved)
		if err != nil {
			return err
		}
		available -= reserved
	}
	rows, err := tx.Query(ctx, runSelect+`WHERE r.host = $1 AND r.status = 'queued'
		ORDER BY r.created_at, r.id FOR UPDATE OF r SKIP LOCKED`, host)
	if err != nil {
		return err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (run, error) { return scanRun(row) })
	if err != nil {
		return err
	}
	for _, r := range runs {
		var estimate int64
		if err := tx.QueryRow(ctx, `SELECT estimated_memory_bytes FROM workflow_runs WHERE id = $1`, r.ID).Scan(&estimate); err != nil {
			return err
		}
		// An oversized repository must not block smaller jobs that fit behind it.
		if !legacy && estimate > available {
			continue
		}
		if err := e.launchOrchestrator(ctx, tx, r); err != nil {
			return err
		}
		available -= estimate
	}
	return nil
}
