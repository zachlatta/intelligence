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
	// RetryWaitLimit is how long past due a start retry may wait, for its host to be ready or for
	// room in its host's queue. Then it is sent if the host is ready and has room (its run kept
	// its reservation), and the run fails, saying why, if not.
	RetryWaitLimit = 5 * time.Minute
)

// retryStart schedules a retry and makes it eligible for the host's capacity queue
// when the backoff has elapsed and the host is ready. The caller holds the queue
// and run locks. A false handled result means the caller should fail the run.
func (e *Engine) retryStart(ctx context.Context, tx pgx.Tx, r run, c fleet.Command, msg, workspaceID string, at time.Time) (handled bool, _ string, _ error) {
	retries, due := r.StartRetries, r.StartRetryAt
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
	overdue := !at.Before(due.Add(RetryWaitLimit))
	ready, err := retryHostReady(ctx, tx, r.Host)
	if overdue && errors.Is(err, pgx.ErrNoRows) {
		ready, err = false, nil // a host that is gone cannot take it: fail the run below
	}
	if err != nil {
		return true, msg, err
	}
	if ready && r.skipsQueue() {
		// Started by hand or immediate, ahead of the queue and whatever the load: its retry is too.
		why := "started by hand: retrying ahead of the queue"
		if !r.StartedByHand {
			why = "immediate priority retry, ahead of the queue"
		}
		_, err := e.dequeueRetry(ctx, tx, r, why)
		return true, msg, err
	}
	if overdue {
		// The retry has waited RetryWaitLimit past its time. Send it if the host is ready and
		// has room (its own reservation was kept), and otherwise fail the run saying why: it
		// must not sit in starting for hours, nor start beyond the host's capacity.
		var why string
		if ready {
			if why, err = e.room(ctx, tx, r.Host, r.ID, r.Repo, r.EstimatedMemory); err != nil {
				return true, msg, err
			}
			if why == "" {
				_, err := e.dequeueRetry(ctx, tx, r, r.Host+" has room for the retry")
				return true, msg, err
			}
			why = r.Host + " still had no room (" + why + ")"
		} else if why, err = retryHostNotReady(ctx, tx, r.Host); err != nil {
			return true, msg, err
		}
		return false, fmt.Sprintf("Start retry %d of %d was due at %s, but %s %s after it was due. Last start error: %s",
			retries+1, StartRetryLimit, due.UTC().Format(time.RFC3339), why, waitLimitWords(), msg), nil
	}
	// Preserve the run's original FIFO priority when it becomes eligible again.
	_, err = tx.Exec(ctx, `UPDATE workflow_runs SET start_retry_queued=$2, queue_reason=''
		WHERE id=$1 AND start_retry_queued IS DISTINCT FROM $2`, r.ID, ready)
	return true, msg, err
}

func retryHostReady(ctx context.Context, tx pgx.Tx, host string) (bool, error) {
	var ready bool
	err := tx.QueryRow(ctx, `SELECT s.online AND s.connected AND h.start_retry_keys
		FROM host_status s JOIN hosts h USING (name) WHERE name=$1`, host).Scan(&ready)
	return ready, err
}

// retryHostNotReady says why host cannot take a retry now, for an error: "host-1 was still
// offline", "host-1's Paseo daemon was still not connected", or "host-1 still did not support
// retry keys".
func retryHostNotReady(ctx context.Context, tx pgx.Tx, host string) (string, error) {
	var online, connected, keys bool
	err := tx.QueryRow(ctx, `SELECT coalesce(s.online, false), coalesce(s.connected, false), coalesce(h.start_retry_keys, false)
		FROM hosts h LEFT JOIN host_status s USING (name) WHERE h.name=$1`, host).Scan(&online, &connected, &keys)
	if errors.Is(err, pgx.ErrNoRows) {
		return host + " was still unknown", nil
	}
	switch {
	case err != nil:
		return "", err
	case !online:
		return host + " was still offline", nil
	case !connected:
		return host + "'s Paseo daemon was still not connected", nil
	case !keys:
		return host + " still did not support retry keys (update its host client)", nil
	}
	return host + " was still not ready", nil
}

func waitLimitWords() string {
	return fmt.Sprintf("%d minutes", int(RetryWaitLimit.Minutes()))
}

// dequeueRetry runs only after the priority queue admits r (or an overdue retry finds room), or
// when r skips the queue (by hand or immediate priority); why says which. Lock the old command so a late success either wins before
// admission or is superseded by this delivery.
func (e *Engine) dequeueRetry(ctx context.Context, tx pgx.Tx, r run, why string) (bool, error) {
	var previous fleet.Command
	err := tx.QueryRow(ctx, `SELECT c.id::text, c.kind, c.idempotency_key
		FROM commands c JOIN workflow_runs r ON r.pending_command=c.id
		WHERE r.id=$1 AND c.status IN ('failed','expired') FOR UPDATE OF c`, r.ID).
		Scan(&previous.ID, &previous.Kind, &previous.IdempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // reconciliation handles a result that won the race
	}
	if err != nil {
		return false, err
	}
	// Reuse Paseo's creation key, but give this delivery a new ID: the host caches completed
	// deliveries, including failures.
	var args json.RawMessage
	if r.Status == FindingProject {
		args, err = projectStartArgs(ctx, tx, r)
	} else {
		args, err = e.orchestratorStartArgs(ctx, tx, r)
	}
	if err != nil {
		return false, err
	}
	at := e.Now()
	next, err := fleet.EnqueueTx(ctx, tx, r.Host, previous.Kind, args, commandTTL, at)
	if err != nil {
		return false, err
	}
	key := previous.IdempotencyKey
	if key == "" {
		key = previous.ID
	}
	if _, err := tx.Exec(ctx, "UPDATE commands SET idempotency_key=$2 WHERE id=$1", next.ID, key); err != nil {
		return false, err
	}
	if err := e.recordDequeue(ctx, tx, r, why); err != nil {
		return false, err
	}
	if err := e.event(ctx, tx, r, "start_retry_sent", nil, nil, fmt.Sprintf("Retry %d of %d", r.StartRetries+1, StartRetryLimit)); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE workflow_runs SET pending_command=$2, launch_command=$2, launched_at=$3,
		start_retries=start_retries+1, start_retry_at=NULL, start_retry_queued=false,
		project_command=CASE WHEN status='finding_project' THEN $2 ELSE project_command END, updated_at=$3 WHERE id=$1`, r.ID, next.ID, at)
	return err == nil, err
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
	if len(ids) > 0 {
		// Failed attempts can release room, and due retries can join the FIFO.
		// Use the same admission path as resource updates and the queue worker.
		failures = append(failures, e.StartQueued(ctx))
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

// RunStallWatchdog bounds each pass and coordinates through row locks in Postgres.
func (e *Engine) RunStallWatchdog(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	runStallWatchdog(ctx, tick.C, e.ReconcileStalls)
}

func runStallWatchdog(ctx context.Context, ticks <-chan time.Time, reconcile func(context.Context) error) {
	for {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := reconcile(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("workflow stall watchdog: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}
