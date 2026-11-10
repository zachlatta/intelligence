package workflow

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// RetryStarts reconciles finished startup commands, including results missed during a
// server restart and expiries with no host update. PostgreSQL holds all retry state.
// Delivery goes through the existing fleet command queue and its host reconnect path.
func (e *Engine) RetryStarts(ctx context.Context) error {
	rows, err := e.p.Query(ctx, `SELECT r.pending_command::text FROM workflow_runs r JOIN commands c ON c.id=r.pending_command
 WHERE r.status IN ('finding_project','starting') AND c.status IN ('failed','expired','succeeded')`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := e.CommandUpdated(ctx, id); err != nil {
			return err
		}
	}
	return nil
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
