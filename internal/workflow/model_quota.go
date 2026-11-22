package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/hostwatch"
)

const (
	// ModelQuotaWaitingOn is the activity_waiting_on value for runs blocked on model quota.
	ModelQuotaWaitingOn = "model quota"

	// ModelQuotaAlertAfter is how long a host must have a run waiting on model quota before
	// Intelligence sends one host-level outage notice.
	ModelQuotaAlertAfter = 30 * time.Minute

	modelQuotaCheckEvery    = time.Minute
	modelQuotaAlertMaxTries = 10
	modelQuotaAlertLockKey  = 0x6d6f64656c71756f // "modelquo"
)

// cleanExclude trims, drops blanks, and deduplicates excluded provider/model routes.
func cleanExclude(exclude []string) []string {
	out := make([]string, 0, len(exclude))
	seen := make(map[string]bool, len(exclude))
	for _, s := range exclude {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func suggestPollCommand(runID, token string, exclude []string) string {
	cmd := "intelligence workflow suggest --run " + runID + " " + token
	if len(exclude) > 0 {
		cmd += " --exclude " + strings.Join(exclude, ",")
	}
	return cmd
}

// openModelWait records that r is waiting for an eligible model in group/token (honoring exclude)
// and marks its activity as waiting on "model quota". Repeated polls keep the original since timestamp.
func (e *Engine) openModelWait(ctx context.Context, tx pgx.Tx, r run, step int, groupID, token string, exclude []string) error {
	now := e.Now()
	exclude = cleanExclude(exclude)
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_model_waits (account_id, run_id, host, step, group_id, token, exclude, since, last_checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (run_id) WHERE resolved_at IS NULL
		DO UPDATE SET step = EXCLUDED.step, group_id = EXCLUDED.group_id, token = EXCLUDED.token, exclude = EXCLUDED.exclude, last_checked_at = EXCLUDED.last_checked_at`,
		r.Account, r.ID, r.Host, step, groupID, token, exclude, now); err != nil {
		return err
	}
	// Set from the stored status, not r: callers such as fillModels at run start hold a run
	// whose status is not yet running, which refreshActivity would treat as not live.
	fallback := "Waiting for an eligible model in " + token
	_, err := tx.Exec(ctx, `UPDATE workflow_runs
		SET activity_waiting_on = CASE WHEN status = 'waiting' THEN activity_waiting_on ELSE $2 END,
		    current_activity = CASE WHEN current_activity = '' THEN $3 ELSE current_activity END,
		    activity_updated_at = CASE WHEN current_activity = '' THEN $4 ELSE activity_updated_at END,
		    activity_eta = CASE WHEN activity_eta_estimated THEN NULL ELSE activity_eta END,
		    activity_eta_estimated = CASE WHEN activity_eta_estimated THEN false ELSE activity_eta_estimated END
		WHERE id = $1`, r.ID, ModelQuotaWaitingOn, fallback, now)
	return err
}

// resolveModelWait closes any open model-quota wait on r. Returns true if an open wait was closed.
func (e *Engine) resolveModelWait(ctx context.Context, tx pgx.Tx, runID, reason string, commandID *string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE workflow_model_waits
		SET resolved_at = $2, resolved_reason = $3, command_id = COALESCE($4::uuid, command_id)
		WHERE run_id = $1 AND resolved_at IS NULL`, runID, e.Now(), reason, commandID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func hasOpenModelWait(ctx context.Context, tx pgx.Tx, runID string) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_model_waits WHERE run_id = $1 AND resolved_at IS NULL)`, runID).Scan(&open)
	return open, err
}

// blockedModelToken returns the group ID, token, and excluded routes for r's current step if r has
// an active model-availability block (an open workflow_model_waits row, or a failed model suggestion
// on the current step with no subsequent successful suggestion for that token).
func blockedModelToken(ctx context.Context, tx pgx.Tx, r run) (groupID, token string, exclude []string, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT group_id::text, token, exclude FROM workflow_model_waits WHERE run_id = $1 AND resolved_at IS NULL`, r.ID).Scan(&groupID, &token, &exclude)
	if err == nil {
		return groupID, token, cleanExclude(exclude), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, false, err
	}
	err = tx.QueryRow(ctx, `SELECT s.group_id::text, s.token,
		       CASE WHEN cardinality(s.exclude) > 0 THEN s.exclude
		            WHEN jsonb_typeof(s.snapshot->'excluded') = 'array' THEN ARRAY(SELECT jsonb_array_elements_text(s.snapshot->'excluded'))
		            ELSE '{}'::text[] END
		FROM workflow_model_suggestions s
		WHERE s.run_id = $1 AND s.step = $2 AND s.provider IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM workflow_model_suggestions ok
		      WHERE ok.run_id = s.run_id AND ok.step = s.step AND ok.token = s.token
		        AND ok.provider IS NOT NULL AND ok.created_at >= s.created_at
		  )
		ORDER BY s.created_at DESC LIMIT 1`, r.ID, r.Step).Scan(&groupID, &token, &exclude)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, false, nil
	}
	if err != nil {
		return "", "", nil, false, err
	}
	return groupID, token, cleanExclude(exclude), true, nil
}

// looksLikeModelAsk reports whether an ask note is only about model-group availability/quota
// (for example, asking permission to bypass the suggester or launch a model directly).
func looksLikeModelAsk(note, token string) bool {
	lower := strings.ToLower(note)
	if token != "" && strings.Contains(lower, strings.ToLower(token)) {
		return true
	}
	for _, phrase := range []string{
		"no eligible model",
		"model suggester",
		"model quota",
		"eligible model",
		"model group",
		"explicit model",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// hasEligibleCandidate reports whether groupID currently has at least one eligible candidate on
// host that is not in exclude.
func hasEligibleCandidate(ctx context.Context, tx pgx.Tx, account int64, groupID, host string, exclude []string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM model_group_candidates
		WHERE account_id = $1 AND group_id = $2 AND host = $3 AND eligible
		  AND NOT ((provider || '/' || model) = ANY($4::text[]))
	)`, account, groupID, host, cleanExclude(exclude)).Scan(&ok)
	return ok, err
}

// ReconcileModelWaits resumes runs whose blocked model group now has an eligible model (including
// runs already parked on a model-only ask), cleans up waits for finished runs, and opens/sends at
// most one host-level outage notice once a host has been waiting past the threshold.
func (e *Engine) ReconcileModelWaits(ctx context.Context) error {
	var failures []error
	if _, err := e.p.Exec(ctx, `UPDATE workflow_model_waits w
		SET resolved_at = $1, resolved_reason = 'finished'
		FROM workflow_runs r
		WHERE r.id = w.run_id AND w.resolved_at IS NULL
		  AND r.status NOT IN ('running', 'waiting', 'stalled')`, e.Now()); err != nil {
		failures = append(failures, fmt.Errorf("cleanup model waits: %w", err))
	}
	if err := e.resumeModelWaits(ctx); err != nil {
		failures = append(failures, err)
	}
	if err := e.resumeParkedModelAsks(ctx); err != nil {
		failures = append(failures, err)
	}
	if err := e.reconcileModelQuotaAlerts(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (e *Engine) resumeModelWaits(ctx context.Context) error {
	rows, err := e.p.Query(ctx, `SELECT w.run_id::text
		FROM workflow_model_waits w
		JOIN workflow_runs r ON r.id = w.run_id
		WHERE w.resolved_at IS NULL
		  AND r.status = 'running'
		  AND r.orchestrator_id <> ''
		  AND EXISTS (
		      SELECT 1 FROM model_group_candidates c
		      WHERE c.account_id = w.account_id AND c.group_id = w.group_id AND c.host = w.host AND c.eligible
		        AND NOT ((c.provider || '/' || c.model) = ANY(w.exclude))
		  )
		ORDER BY w.since, w.run_id
		LIMIT 100`)
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
			r, err := scanRun(tx.QueryRow(ctx, runSelect+`WHERE r.id = $1 AND r.status = 'running' AND r.orchestrator_id <> '' FOR UPDATE OF r SKIP LOCKED`, id))
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			var groupID, token string
			var exclude []string
			err = tx.QueryRow(ctx, `SELECT group_id::text, token, exclude FROM workflow_model_waits WHERE run_id = $1 AND resolved_at IS NULL`, r.ID).Scan(&groupID, &token, &exclude)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := lockModels(ctx, tx, r.Account); err != nil {
				return err
			}
			eligible, err := hasEligibleCandidate(ctx, tx, r.Account, groupID, r.Host, exclude)
			if err != nil || !eligible {
				return err
			}
			sug, err := e.suggestModel(ctx, tx, r, r.Step, token, exclude)
			if err != nil || !sug.Eligible {
				return err
			}
			stepTitle := ""
			if r.Step >= 0 && r.Step < len(r.Def.Steps) {
				stepTitle = r.Def.Steps[r.Step].Title
			}
			text := fmt.Sprintf("Intelligence: an eligible model is available in %s again. Continue step %d (%s) in this turn.\n\nModel suggestion:\n- %s: %s%s",
				token, r.Step+1, stepTitle, token, sug.Note, sameTurnReminder)
			args, _ := json.Marshal(hostclient.AgentArgs{AgentID: r.Orchestrator, Text: text, Behavior: hostclient.Queue})
			c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.SendMessageKind, args, replyTTL, e.Now())
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_model_waits SET command_id = $2::uuid, resolved_reason = 'resumed'
				WHERE id = (SELECT id FROM workflow_model_waits WHERE run_id = $1 ORDER BY id DESC LIMIT 1)`, r.ID, c.ID); err != nil {
				return err
			}
			if err := e.event(ctx, tx, r, "resume", ptr(r.Step), nil, fmt.Sprintf("Model quota returned for %s; resumed the orchestrator.", token)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET idle_since = NULL, idle_observation = '', updated_at = $2 WHERE id = $1`, r.ID, e.Now()); err != nil {
				return err
			}
			return e.refreshActivity(ctx, tx, r)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("resume model wait %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) resumeParkedModelAsks(ctx context.Context) error {
	rows, err := e.p.Query(ctx, `SELECT r.id::text
		FROM workflow_runs r
		WHERE r.status = 'waiting'
		  AND r.orchestrator_id <> ''
		  AND r.note <> ''
		  AND EXISTS (
		      SELECT 1 FROM workflow_model_suggestions s
		      WHERE s.run_id = r.id AND s.step = r.step AND s.provider IS NULL
		  )
		ORDER BY r.updated_at, r.id
		LIMIT 100`)
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
			r, err := scanRun(tx.QueryRow(ctx, runSelect+`WHERE r.id = $1 AND r.status = 'waiting' AND r.orchestrator_id <> '' FOR UPDATE OF r SKIP LOCKED`, id))
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if r.Step < 0 || r.Step >= len(r.Def.Steps) || r.Def.Steps[r.Step].Kind == KindSignoff {
				return nil
			}
			var note string
			if err := tx.QueryRow(ctx, `SELECT note FROM workflow_runs WHERE id = $1`, r.ID).Scan(&note); err != nil {
				return err
			}
			groupID, token, exclude, blocked, err := blockedModelToken(ctx, tx, r)
			if err != nil || !blocked || !looksLikeModelAsk(note, token) {
				return err
			}
			if err := lockModels(ctx, tx, r.Account); err != nil {
				return err
			}
			eligible, err := hasEligibleCandidate(ctx, tx, r.Account, groupID, r.Host, exclude)
			if err != nil || !eligible {
				return err
			}
			sug, err := e.suggestModel(ctx, tx, r, r.Step, token, exclude)
			if err != nil || !sug.Eligible {
				return err
			}
			stepTitle := r.Def.Steps[r.Step].Title
			text := fmt.Sprintf("Intelligence: your model-availability ask for %s was automatically resolved because an eligible model is available again. Continue step %d (%s) in this turn.\n\nModel suggestion:\n- %s: %s%s",
				token, r.Step+1, stepTitle, token, sug.Note, sameTurnReminder)
			args, _ := json.Marshal(hostclient.AgentArgs{AgentID: r.Orchestrator, Text: text, Behavior: hostclient.Queue})
			c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.SendMessageKind, args, replyTTL, e.Now())
			if err != nil {
				return err
			}
			if _, err := e.resolveModelWait(ctx, tx, r.ID, "ask_resolved", &c.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET status = $2, note = '', idle_since = NULL, idle_observation = '', updated_at = $3 WHERE id = $1`, r.ID, Running, e.Now()); err != nil {
				return err
			}
			if err := e.event(ctx, tx, r, "resume", ptr(r.Step), nil, fmt.Sprintf("Resolved model-availability ask for %s: an eligible model returned; resumed the orchestrator.", token)); err != nil {
				return err
			}
			r.Status = Running
			return e.refreshActivity(ctx, tx, r)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("resume parked ask %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) reconcileModelQuotaAlerts(ctx context.Context) error {
	threshold := e.ModelQuotaAlertAfter
	if threshold == 0 {
		threshold = ModelQuotaAlertAfter
	}
	now := e.Now()
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(modelQuotaAlertLockKey)).Scan(&got); err != nil || !got {
			return err
		}
		// Close alerts for hosts that no longer have any live runs waiting on model quota.
		if _, err := tx.Exec(ctx, `UPDATE model_quota_alerts a
			SET recovered_at = $1
			WHERE a.recovered_at IS NULL
			  AND NOT EXISTS (
			      SELECT 1 FROM workflow_model_waits w
			      JOIN workflow_runs r ON r.id = w.run_id
			      WHERE w.account_id = a.account_id AND w.host = a.host
			        AND w.resolved_at IS NULL
			        AND r.status IN ('running', 'waiting', 'stalled')
			  )`, now); err != nil {
			return err
		}
		// Open at most one alert per (account_id, host) once any wait has lasted >= threshold.
		if _, err := tx.Exec(ctx, `WITH waiting AS (
				SELECT w.account_id, w.host, w.run_id, w.token, w.since
				FROM workflow_model_waits w
				JOIN workflow_runs r ON r.id = w.run_id
				WHERE w.resolved_at IS NULL AND r.status IN ('running', 'waiting', 'stalled')
			), grouped AS (
				SELECT account_id, host,
				       min(since) AS since,
				       array_agg(DISTINCT token ORDER BY token) AS tokens,
				       count(*)::int AS blocked_runs,
				       array_agg(run_id ORDER BY since, run_id) AS run_ids
				FROM waiting
				GROUP BY account_id, host
				HAVING min(since) <= $1::timestamptz - make_interval(secs => $2::float8)
			)
			INSERT INTO model_quota_alerts (account_id, host, since, opened_at, tokens, blocked_runs, run_ids, notify)
			SELECT g.account_id, g.host, g.since, $1::timestamptz, g.tokens, g.blocked_runs, g.run_ids,
			       EXISTS (SELECT 1 FROM accounts acc WHERE acc.id = g.account_id AND acc.username = $3::text AND $3::text <> '')
			FROM grouped g
			WHERE NOT EXISTS (
				SELECT 1 FROM model_quota_alerts a
				WHERE a.account_id = g.account_id AND a.host = g.host AND a.recovered_at IS NULL
			)
			ON CONFLICT DO NOTHING`, now, threshold.Seconds(), e.NotifyAccount); err != nil {
			return err
		}
		if e.Notify == nil {
			return nil
		}
		return e.sendModelQuotaAlerts(ctx, tx, now)
	})
}

type quotaAlert struct {
	id          int64
	host        string
	since       time.Time
	tokens      []string
	blockedRuns int
	runIDs      []string
}

func (e *Engine) sendModelQuotaAlerts(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, host, since, tokens, blocked_runs,
		run_ids::text[]
		FROM model_quota_alerts
		WHERE notify AND recovered_at IS NULL AND alert_sent_at IS NULL AND alert_attempts < $1
		ORDER BY id`, modelQuotaAlertMaxTries)
	if err != nil {
		return err
	}
	alerts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (quotaAlert, error) {
		var a quotaAlert
		err := r.Scan(&a.id, &a.host, &a.since, &a.tokens, &a.blockedRuns, &a.runIDs)
		return a, err
	})
	if err != nil {
		return err
	}
	for _, a := range alerts {
		n, err := e.quotaAlertNotice(ctx, tx, a, now)
		if err != nil {
			return err
		}
		sendErr := e.Notify.Notify(ctx, n)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		msg, sent := "", any(now)
		if sendErr != nil {
			msg, sent = hostwatch.Clip(sendErr.Error(), 500), nil
		}
		text := n.Title + "\n" + n.Body
		if _, err := tx.Exec(ctx, `UPDATE model_quota_alerts
			SET alert_text = $2, alert_sent_at = $3, alert_error = $4, alert_attempts = alert_attempts + 1
			WHERE id = $1`, a.id, text, sent, msg); err != nil {
			return err
		}
		if sendErr != nil {
			log.Printf("model quota alert: notify %s: %v", a.host, sendErr)
		}
	}
	return nil
}

func (e *Engine) quotaAlertNotice(ctx context.Context, tx pgx.Tx, a quotaAlert, now time.Time) (hostwatch.Notice, error) {
	var labels []string
	limit := min(5, len(a.runIDs))
	for _, id := range a.runIDs[:limit] {
		var summary, title string
		if err := tx.QueryRow(ctx, `SELECT task_summary, title FROM workflow_runs WHERE id = $1`, id).Scan(&summary, &title); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return hostwatch.Notice{}, err
		}
		label := strings.TrimSpace(summary)
		if label == "" {
			label = strings.TrimSpace(title)
		}
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		if label != "" {
			labels = append(labels, fmt.Sprintf("%s (%s)", short, hostwatch.Clip(label, 40)))
		} else {
			labels = append(labels, short)
		}
	}
	runList := strings.Join(labels, ", ")
	if extra := len(a.runIDs) - limit; extra > 0 {
		runList += fmt.Sprintf(", and %d more", extra)
	}
	runsWord := fmt.Sprintf("%d runs", a.blockedRuns)
	if a.blockedRuns == 1 {
		runsWord = "1 run"
	}
	body := fmt.Sprintf("No eligible model in %s for %s. %s waiting: %s. Runs will resume automatically when quota returns.",
		strings.Join(a.tokens, ", "), hostwatch.Span(now.Sub(a.since)), runsWord, runList)
	return hostwatch.Notice{
		Title:    hostwatch.Clip("Model quota unavailable on "+a.host, 60),
		Body:     body,
		ThreadID: "intelligence-model-quota",
		Level:    "active",
	}, nil
}

// RunModelQuotaWatcher checks every minute until ctx ends. More than one instance may safely run it.
func (e *Engine) RunModelQuotaWatcher(ctx context.Context) {
	tick := time.NewTicker(modelQuotaCheckEvery)
	defer tick.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := e.ReconcileModelWaits(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("model quota watcher: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
