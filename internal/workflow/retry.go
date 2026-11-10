package workflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// RetryWaitLimit bounds each queued start retry, including daemon and host upgrade waits.
const RetryWaitLimit = 24 * time.Hour

// RetryStarts reconciles finished startup commands, including results missed during a
// server restart and expiries with no host update. PostgreSQL holds all retry state.
// Delivery goes through the existing fleet command queue and its host reconnect path.
func (e *Engine) RetryStarts(ctx context.Context) error {
	// Claim normally expires commands on an open host stream. Offline hosts have
	// no Claim call, so reconcile overdue startup deliveries here as well. The
	// predicate serializes with host results and Claim; successful results survive.
	if _, err := e.p.Exec(ctx, `WITH expired AS (
		UPDATE commands c SET status='expired', finished_at=c.expires_at,
			error=CASE c.status WHEN 'queued' THEN 'expired before the host was connected to receive it'
			ELSE 'expired with no result from the host: it may or may not have run' END
		FROM workflow_runs r WHERE r.pending_command=c.id AND r.status IN ('finding_project','starting')
			AND c.status IN ('queued','sent','running') AND c.expires_at <= $1
		RETURNING c.id)
		SELECT pg_notify('fleet','command:' || id) FROM expired`, e.Now()); err != nil {
		return err
	}
	rows, err := e.p.Query(ctx, `SELECT r.pending_command::text FROM workflow_runs r JOIN commands c ON c.id=r.pending_command
 WHERE r.status IN ('finding_project','starting') AND c.status IN ('failed','expired','succeeded')
 ORDER BY r.created_at, r.id`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := e.CommandUpdated(ctx, id); err != nil {
			failures = append(failures, fmt.Errorf("command %s: %w", id, err))
		}
	}
	// The worker logs the aggregate, including every bad command's ID, after all
	// other runs have been reconciled. One corrupt run cannot starve the queue.
	return errors.Join(failures...)
}

// RunStartRetries stops with the server. More than one instance may safely run it.
func (e *Engine) RunStartRetries(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := e.RetryStarts(ctx); err != nil && ctx.Err() == nil {
			log.Printf("workflow start retries: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
