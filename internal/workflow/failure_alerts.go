package workflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostwatch"
)

// failureAlertLockKey serializes SendFailureAlerts across server instances.
const failureAlertLockKey = 0x77666661696c616c // "wffailal"

// maxFailureAlertAttempts bounds retries when PDW refuses a notification.
const maxFailureAlertAttempts = 10

// recordFailureAlert records one pending phone alert for a failed answer-expected run
// and queues a Web Push notification for the owning account when Web Push is enabled.
func (e *Engine) recordFailureAlert(ctx context.Context, tx pgx.Tx, r run, errMsg string) error {
	if e.Push != nil {
		var summary string
		_ = tx.QueryRow(ctx, "SELECT task_summary FROM workflow_runs WHERE id = $1", r.ID).Scan(&summary)
		addPush(ctx, failPush(r.Account, r.ID, runPushLabel(summary, r.Title), errMsg))
	}
	if !r.AnswerExpected {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_failure_alerts
		(run_id, account_id, workflow_id, title, error, opened_at, notify)
		SELECT r.id, r.account_id, r.workflow_id, r.title, $2, $3,
		       EXISTS (SELECT 1 FROM accounts a WHERE a.id = r.account_id AND a.username = $4::text AND $4::text <> '')
		FROM workflow_runs r
		WHERE r.id = $1 AND r.answer_expected
		ON CONFLICT (run_id) DO NOTHING`, r.ID, strings.TrimSpace(errMsg), e.Now(), e.NotifyAccount)
	return err
}

// sendFailureAlertsBestEffort sends any pending failure alerts right away, logging errors rather
// than failing the caller's completed state transition.
func (e *Engine) sendFailureAlertsBestEffort(ctx context.Context) {
	if e.Notify == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := e.SendFailureAlerts(bounded); err != nil && ctx.Err() == nil {
		log.Printf("workflow failure alerts: %v", err)
	}
}

type pendingFailureAlert struct {
	runID    string
	title    string
	errMsg   string
	workflow string
	trigger  string
}

// SendFailureAlerts pushes pending phone notifications for failed answer-expected runs through
// Notify (PDW). One server instance at a time runs it.
func (e *Engine) SendFailureAlerts(ctx context.Context) error {
	if e.Notify == nil {
		return nil
	}
	now := e.Now()
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(failureAlertLockKey)).Scan(&got); err != nil || !got {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.run_id::text, a.title, a.error, w.name, coalesce(v.definition->>'trigger', '')
			FROM workflow_run_failure_alerts a
			JOIN workflow_runs r ON r.id = a.run_id
			JOIN workflows w ON w.id = a.workflow_id
			JOIN workflow_versions v ON v.workflow_id = r.workflow_id AND v.version = r.version
			WHERE a.notify AND a.alert_sent_at IS NULL AND a.alert_attempts < $1
			ORDER BY a.opened_at, a.run_id`, maxFailureAlertAttempts)
		if err != nil {
			return err
		}
		alerts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingFailureAlert, error) {
			var a pendingFailureAlert
			err := r.Scan(&a.runID, &a.title, &a.errMsg, &a.workflow, &a.trigger)
			return a, err
		})
		if err != nil {
			return err
		}
		for _, a := range alerts {
			n := failureNotice(a)
			sendErr := e.Notify.Notify(ctx, n)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			errText, sent := "", any(now)
			if sendErr != nil {
				errText, sent = clipRunes(sendErr.Error(), 500), nil
			}
			text := n.Title + "\n" + n.Body
			if _, err := tx.Exec(ctx, `UPDATE workflow_run_failure_alerts
				SET alert_text = $2, alert_sent_at = $3, alert_error = $4, alert_attempts = alert_attempts + 1
				WHERE run_id = $1`, a.runID, text, sent, errText); err != nil {
				return err
			}
			if sendErr != nil {
				log.Printf("workflow failure alert %s: %v", a.runID, sendErr)
			}
		}
		return nil
	})
}

var workspaceWrapperRe = regexp.MustCompile(`made workspace [^,]+, but its agent:\s*`)

// plainFailureError turns internal command wrappers into a plain-English explanation of why the run failed.
func plainFailureError(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "The run stopped with an error."
	}
	s = workspaceWrapperRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "agent.create.request: ", "")
	s = strings.ReplaceAll(s, "workspace.create.request: ", "")
	s = strings.TrimPrefix(s, "could not start the orchestrator: ")
	s = strings.TrimPrefix(s, "could not start the project finder: ")
	s = strings.TrimSpace(s)
	if s == "" {
		return "The run could not start."
	}
	if strings.HasPrefix(raw, "could not start the orchestrator:") {
		s = "Couldn't start the orchestrator: " + s
	} else if strings.HasPrefix(raw, "could not start the project finder:") {
		s = "Couldn't start the project finder: " + s
	}
	s = clipRunes(s, 260)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "…") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}

func failureNotice(a pendingFailureAlert) hostwatch.Notice {
	noun := "Workflow run"
	switch a.trigger {
	case TriggerVoiceMemo:
		noun = "Voice memo"
	case TriggerEmail:
		noun = "Email"
	}
	title := noun + " failed"
	if t := strings.TrimSpace(a.title); t != "" {
		title += ": " + t
	}
	body := fmt.Sprintf("%s: %s Fix the workflow to re-run it automatically, or open the run to retry.",
		a.workflow, plainFailureError(a.errMsg))
	return hostwatch.Notice{
		Title:    clipRunes(title, 60),
		Body:     clipRunes(body, 400),
		ThreadID: "intelligence-workflow-failures",
		Level:    "time-sensitive",
	}
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
