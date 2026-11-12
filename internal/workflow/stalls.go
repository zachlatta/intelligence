package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Startup retries and admission own their states. Recheck this predicate under
// the row lock so another reconciler cannot change eligibility between the scan and delivery.
const stallEligible = `r.status='running' AND r.pending_command IS NULL
 AND r.start_retry_at IS NULL AND NOT r.start_retry_queued`

// ReconcileStalls follows RetryStarts: isolate each run and report all failures after
// processing the batch. Run locks serialize recovery with moves, replies and cancellation.
// It never advances a step or starts a replacement agent.
func (e *Engine) ReconcileStalls(ctx context.Context) error {
	rows, err := e.p.Query(ctx, `SELECT r.id::text FROM workflow_runs r WHERE `+stallEligible+`
 ORDER BY r.stall_checked_at NULLS FIRST,r.id LIMIT 100`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
			r, err := scanRun(tx.QueryRow(ctx, runSelect+`WHERE r.id=$1 AND `+stallEligible+` FOR UPDATE OF r SKIP LOCKED`, id))
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if r.Step < 0 || r.Step >= len(r.Def.Steps) {
				return fmt.Errorf("invalid current step %d", r.Step)
			}
			return e.reconcileStall(ctx, tx, r)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("run %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// idleSnapshot includes descendants even when workers use another workspace. UNION
// (rather than UNION ALL) makes malformed parent cycles finite.
const idleSnapshot = `WITH RECURSIVE tree AS (
 SELECT a.* FROM agents a WHERE a.host=$1 AND a.id=$2
 UNION
 SELECT a.* FROM agents a JOIN tree p ON a.host=p.host AND a.parent_id=p.id
 ) SELECT
 EXISTS(SELECT 1 FROM hosts h WHERE h.name=$1 AND h.connected AND h.status_error=''
   AND h.stream_id IS NOT NULL AND h.last_seen > $3::timestamptz-interval '1 minute'
   AND h.report_received_at > $3::timestamptz-interval '1 minute'),
 EXISTS(SELECT 1 FROM tree WHERE id=$2 AND status='idle'),
 EXISTS(SELECT 1 FROM tree WHERE (id<>$2 AND status IN ('running','initializing')) OR attention_reason='permission')
 OR EXISTS(SELECT 1 FROM permission_resolutions p JOIN tree t ON p.host=t.host AND p.agent_id=t.id
   WHERE p.status IN ('pending','error','stuck') AND p.cleared_at IS NULL),
 EXISTS(SELECT 1 FROM commands c WHERE c.host=$1 AND c.status IN ('queued','sent','running')
  AND c.expires_at>$3 AND (c.args->>'agentId' IN (SELECT id FROM tree)
    OR c.args->>'workspaceId'=$4)),
 coalesce((SELECT jsonb_agg(jsonb_build_array(id,status,attention_reason='permission',updated_at,last_user_message_at) ORDER BY id)::text FROM tree),'')`

func (e *Engine) reconcileStall(ctx context.Context, tx pgx.Tx, r run) error {
	now := e.Now()
	if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET stall_checked_at=$2 WHERE id=$1", r.ID, now); err != nil {
		return err
	}
	var fresh, idle, blocked, pending bool
	var observation string
	if err := tx.QueryRow(ctx, idleSnapshot, r.Host, r.Orchestrator, now, r.WorkspaceID).Scan(&fresh, &idle, &blocked, &pending, &observation); err != nil {
		return err
	}
	var previous string
	var since *time.Time
	var event *int64
	var starting bool
	if err := tx.QueryRow(ctx, `SELECT idle_observation,idle_since,pending_command IS NOT NULL,
 (SELECT event_id FROM workflow_run_latest_transition WHERE run_id=$1) FROM workflow_runs WHERE id=$1`, r.ID).Scan(&previous, &since, &starting, &event); err != nil {
		return err
	}
	if pending {
		return nil
	}
	// Legacy or seeded runs may have no lifecycle history. Without an epoch
	// there is no durable key for a continuation; keep the check timestamp but
	// clear any idle window until a real transition supplies one.
	if event == nil || !fresh || !idle || blocked || starting {
		_, err := tx.Exec(ctx, "UPDATE workflow_runs SET idle_since=NULL,idle_observation='' WHERE id=$1", r.ID)
		return err
	}
	observation += ":" + strconv.FormatInt(*event, 10)
	if since == nil || observation != previous {
		_, err := tx.Exec(ctx, "UPDATE workflow_runs SET idle_since=$2,idle_observation=$3 WHERE id=$1", r.ID, now, observation)
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM workflow_run_continuations WHERE run_id=$1 AND event_id=$2", r.ID, *event).Scan(&count); err != nil {
		return err
	}
	delay := 2 * time.Minute
	if count == 1 {
		delay = 10 * time.Minute
	}
	if count >= 2 {
		delay = 12 * time.Minute
	}
	if now.Sub(*since) < delay {
		return nil
	}
	if count >= 2 {
		reason := "Stalled: orchestrator remained idle without progress after two continuation attempts. Inspect worker outcomes before continuing."
		if err := e.event(ctx, tx, r, "stalled", ptr(r.Step), nil, reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status='stalled',error=$2,updated_at=$3 WHERE id=$1`, r.ID, reason, now)
		return err
	}
	return e.continueRun(ctx, tx, r, *event, count+1)
}

func (e *Engine) continueRun(ctx context.Context, tx pgx.Tx, r run, event int64, attempt int) error {
	d, err := e.fillModels(ctx, tx, r, r.Step)
	if err != nil {
		return err
	}
	text := "Continue this workflow in the same turn. Inspect worker outcomes before moving; follow the current step and all gates. Never skip a gate or repeat an external action without checking its result.\n\n" + StepMessage(d, r.Step, r.vars()) + sameTurnReminder
	args, _ := json.Marshal(hostclient.AgentArgs{AgentID: r.Orchestrator, Text: text, Behavior: "queue"})
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.SendMessageKind, args, time.Minute, e.Now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_run_continuations(run_id,account_id,event_id,attempt,command_id,created_at) VALUES($1,$2,$3,$4,$5,$6)`, r.ID, r.Account, event, attempt, c.ID, e.Now())
	if err != nil {
		return err
	}
	if err := e.event(ctx, tx, r, "continuation", ptr(r.Step), nil, fmt.Sprintf("Continuation %d of 2: asked the existing orchestrator to continue the current step.", attempt)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_runs SET updated_at=$2 WHERE id=$1", r.ID, e.Now())
	return err
}

// Resume explicitly starts a new recovery generation on the existing orchestrator.
// Missing, busy and permission-blocked agents must be inspected rather than replaced.
func (e *Engine) Resume(ctx context.Context, account int64, id string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := e.lockRun(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if r.Status != Stalled {
			return conflict("this run is not stalled (%s)", r.Status)
		}
		var fresh, idle, blocked, pending bool
		var observation string
		if err := tx.QueryRow(ctx, idleSnapshot, r.Host, r.Orchestrator, e.Now(), r.WorkspaceID).Scan(&fresh, &idle, &blocked, &pending, &observation); err != nil {
			return err
		}
		if !fresh || !idle || blocked || pending {
			return conflict("Resume needs a fresh online host, an idle orchestrator, no active or permission-blocked worker and no pending command. Inspect the existing agents first.")
		}
		if err := e.event(ctx, tx, r, "resume", ptr(r.Step), nil, "Resumed the existing orchestrator."); err != nil {
			return err
		}
		var event int64
		if err := tx.QueryRow(ctx, "SELECT event_id FROM workflow_run_latest_transition WHERE run_id=$1", r.ID).Scan(&event); err != nil {
			return err
		}
		if err := e.continueRun(ctx, tx, r, event, 1); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET status='running',error='',updated_at=$2,idle_since=NULL,idle_observation='' WHERE id=$1`, r.ID, e.Now())
		return err
	})
}

// unstall records explicit activity, not another watchdog attempt. It must share
// the caller's transaction so rejected input or invalid moves leave the stall intact.
func (e *Engine) unstall(ctx context.Context, tx pgx.Tx, r run, why string) error {
	if r.Status != Stalled {
		return nil
	}
	if err := e.event(ctx, tx, r, "resume", ptr(r.Step), nil, why); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status='running',error='',idle_since=NULL,idle_observation='',updated_at=$2 WHERE id=$1`, r.ID, e.Now())
	return err
}
