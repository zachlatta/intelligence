package workflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// AutoRetryWindow is how recently a failed-to-start run must have failed to be re-run automatically
// when its workflow gets a new version.
const AutoRetryWindow = 24 * time.Hour

// RetryRun starts a new run of account's failed run's workflow (on its latest version) with the
// same input, links the two runs, and settles the failed run's answer expectation onto the retry.
func (e *Engine) RetryRun(ctx context.Context, account int64, runID string) (string, error) {
	if !uuidRe.MatchString(runID) {
		return "", ErrNotFound
	}
	var newID string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var workflowID string
		err := tx.QueryRow(ctx, `SELECT workflow_id::text FROM workflow_runs WHERE id = $1 AND account_id = $2`, runID, account).Scan(&workflowID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var deleted bool
		var host string
		err = tx.QueryRow(ctx, `SELECT w.deleted_at IS NOT NULL, coalesce(v.definition->>'host', '')
			FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 AND w.account_id = $2 FOR SHARE OF w`, workflowID, account).Scan(&deleted, &host)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if deleted {
			return conflict("this run's workflow was deleted")
		}
		if host != "" {
			if err := lockQueue(ctx, tx, host); err != nil {
				return err
			}
		}
		r, err := e.lockRun(ctx, tx, account, runID)
		if err != nil {
			return err
		}
		if r.Status != Failed {
			return conflict("only a failed run can be retried (%s)", strings.ReplaceAll(r.Status, "_", " "))
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT id::text FROM workflow_runs WHERE retried_from_run_id = $1 ORDER BY created_at DESC LIMIT 1`, r.ID).Scan(&existing)
		if err == nil {
			return conflict("this run was already retried as run %s", existing)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		newID, err = e.retryRunLocked(ctx, tx, account, r, false)
		return err
	})
	if err == nil {
		e.sendFailureAlertsBestEffort(ctx)
	}
	return newID, err
}

// retryRunLocked starts a new run on failed's workflow's latest version with the same input.
// The caller holds the workflow lock, the target host's queue lock, and failed's row lock.
func (e *Engine) retryRunLocked(ctx context.Context, tx pgx.Tx, account int64, failed run, automatic bool) (string, error) {
	newID, err := e.StartRunTx(ctx, tx, account, failed.WorkflowID, failed.Input, time.Time{})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET retried_from_run_id = $2 WHERE id = $1`, newID, failed.ID); err != nil {
		return "", err
	}
	var newVersion int
	if err := tx.QueryRow(ctx, `SELECT version FROM workflow_runs WHERE id = $1`, newID).Scan(&newVersion); err != nil {
		return "", err
	}
	retriedText := fmt.Sprintf("Retried as run %s on %s v%d.", newID, failed.Name, newVersion)
	reason := "Retried as run " + newID
	if automatic {
		retriedText = fmt.Sprintf("Automatically retried as run %s on %s v%d.", newID, failed.Name, newVersion)
		reason = "Automatically retried as run " + newID
	}
	if err := e.event(ctx, tx, failed, "retried", nil, nil, retriedText); err != nil {
		return "", err
	}
	retryOfRun := run{ID: newID, Account: account}
	if err := e.event(ctx, tx, retryOfRun, "retry_of", nil, nil, fmt.Sprintf("Retry of failed run %s.", failed.ID)); err != nil {
		return "", err
	}
	if failed.AnswerExpected {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_answer_resolutions (run_id, account_id, kind, steered_to, reason, at)
			VALUES ($1, $2, 'steered', $3, $4, $5) ON CONFLICT (run_id) DO NOTHING`,
			failed.ID, account, newID, reason, e.Now()); err != nil {
			return "", err
		}
	}
	return newID, nil
}

// retryFailedRunsForWorkflow automatically re-runs recent failed-to-start runs of workflowID when a
// new workflow version is saved. Each run retries in its own savepoint so an unstartable old run
// never blocks saving the workflow.
func (e *Engine) retryFailedRunsForWorkflow(ctx context.Context, tx pgx.Tx, account int64, workflowID string, newVersion int, host string) error {
	if host != "" {
		if err := lockQueue(ctx, tx, host); err != nil {
			return err
		}
	}
	since := e.Now().Add(-AutoRetryWindow)
	rows, err := tx.Query(ctx, `SELECT r.id::text
		FROM workflow_runs r
		WHERE r.workflow_id = $1 AND r.account_id = $2
		  AND r.status = $3
		  AND r.orchestrator_id = ''
		  AND r.version < $4
		  AND r.finished_at >= $5
		  AND NOT EXISTS (SELECT 1 FROM workflow_runs r2 WHERE r2.retried_from_run_id = r.id)
		  AND NOT EXISTS (SELECT 1 FROM workflow_run_answers a WHERE a.run_id = r.id AND a.checked)
		  AND NOT EXISTS (SELECT 1 FROM workflow_run_answer_resolutions s WHERE s.run_id = r.id)
		ORDER BY r.created_at, r.id
		FOR UPDATE OF r`, workflowID, account, Failed, newVersion, since)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1", id))
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			_, err := e.retryRunLocked(ctx, sp, account, r, true)
			return err
		})
		if err != nil {
			var ref *Refusal
			var disabled *fleet.DisabledProviderError
			if errors.As(err, &ref) || errors.As(err, &disabled) {
				log.Printf("workflow auto-retry %s: %v", id, err)
				continue
			}
			return err
		}
	}
	return nil
}
