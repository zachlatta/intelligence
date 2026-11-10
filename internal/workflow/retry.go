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
	at := e.Now()
	rows, err := e.p.Query(ctx, `SELECT r.pending_command::text FROM workflow_runs r JOIN commands c ON c.id=r.pending_command
 WHERE r.status IN ('finding_project','starting')
 AND (c.status IN ('failed','expired','succeeded') OR c.expires_at <= $1)
 ORDER BY r.created_at, r.id`, at)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// A host with no stream never calls Claim. Use the same fleet transition
	// before reconciling its overdue startup commands.
	if err := e.reg.ExpireCommands(ctx, ids, at); err != nil {
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
