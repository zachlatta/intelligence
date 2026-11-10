package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

const (
	// StartRetryLimit is how many times a failed or expired start is tried again.
	StartRetryLimit = 3
	// RetryWaitLimit bounds each queued start retry, including daemon and host upgrade waits.
	RetryWaitLimit = 24 * time.Hour
)

// retryStart follows a failed or expired start command c of r (msg says why): it queues a
// retry, sends a due one once the host can take it, or waits. handled is false when r has
// no retries left or its queued retry timed out; the caller then fails the run with msg.
func (e *Engine) retryStart(ctx context.Context, tx pgx.Tx, r run, c fleet.Command, msg, workspaceID string, at time.Time) (handled bool, _ string, _ error) {
	var retries int
	var due *time.Time
	if err := tx.QueryRow(ctx, "SELECT start_retries, start_retry_at FROM workflow_runs WHERE id=$1", r.ID).Scan(&retries, &due); err != nil {
		return false, msg, err
	}
	if retries >= StartRetryLimit {
		return false, msg, nil
	}
	if due == nil {
		failedAt := at
		// An offline host cannot report expiry. Count the recovery wait from
		// the command's actual expiry, even after a long server outage.
		if c.Status == hostclient.CommandExpired && c.ExpiresAt.Before(at) {
			failedAt = c.ExpiresAt
		}
		nextAt := failedAt.Add(15 * time.Second * time.Duration(1<<retries))
		text := fmt.Sprintf("Retry %d of %d at %s. %s", retries+1, StartRetryLimit, nextAt.UTC().Format(time.RFC3339), msg)
		if err := e.event(ctx, tx, r, "start_retry", nil, nil, text); err != nil {
			return true, msg, err
		}
		_, err := tx.Exec(ctx, "UPDATE workflow_runs SET error=$2, start_retry_at=$3, updated_at=$4, workspace_id=$5 WHERE id=$1", r.ID, msg, nextAt, at, workspaceID)
		return true, msg, err
	}
	if at.Before(*due) {
		return true, msg, nil
	}
	if !at.Before(due.Add(RetryWaitLimit)) {
		return false, "Start retry timed out after 24 hours waiting for the Paseo daemon to reconnect or the host to support retry keys. Last start error: " + msg, nil
	}
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT s.online AND s.connected AND h.start_retry_keys
		FROM host_status s JOIN hosts h USING (name) WHERE name=$1`, r.Host).Scan(&ready); err != nil || !ready {
		return true, msg, err
	}
	// Reuse Paseo's creation key, but give this delivery a new ID: the host caches completed
	// deliveries, including failures. The run lock serializes concurrent workers and leaves
	// old/late command updates detached.
	var args json.RawMessage
	var err error
	if r.Status == FindingProject {
		args, err = projectStartArgs(ctx, tx, r)
	} else {
		args, err = orchestratorStartArgs(r)
	}
	if err != nil {
		return true, msg, err
	}
	next, err := fleet.EnqueueTx(ctx, tx, r.Host, c.Kind, args, commandTTL, at)
	if err != nil {
		return true, msg, err
	}
	key := c.IdempotencyKey
	if key == "" {
		key = c.ID
	}
	if _, err := tx.Exec(ctx, "UPDATE commands SET idempotency_key=$2 WHERE id=$1", next.ID, key); err != nil {
		return true, msg, err
	}
	if err := e.event(ctx, tx, r, "start_retry_sent", nil, nil, fmt.Sprintf("Retry %d of %d", retries+1, StartRetryLimit)); err != nil {
		return true, msg, err
	}
	_, err = tx.Exec(ctx, `UPDATE workflow_runs SET pending_command=$2, start_retries=start_retries+1, start_retry_at=NULL,
		project_command=CASE WHEN status='finding_project' THEN $2 ELSE project_command END, updated_at=$3 WHERE id=$1`, r.ID, next.ID, at)
	return true, msg, err
}

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
