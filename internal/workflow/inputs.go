package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// An input is Zach's input to a run's orchestrator: his reply to its ask, or more input (an email in
// the run's thread or a note from its run page). workflow_run_input_deliveries links it to every
// message that carried it. A host whose Paseo daemon is down fails messages it is sent, so an
// input recorded then can fail to
// arrive (run 2c75da8c, 2026-11-19: the orchestrator asked the answered question again and the
// reply's instruction was lost). Such an input goes again, verbatim and once delivered never again,
// with the next message to the orchestrator: a watchdog continuation, Resume or more input; and
// an ask while a reply is undelivered gets the reply instead of going to Zach.
type input struct {
	Event int64
	At    time.Time
	Kind  string // reply, message or input
	Text  string // the message first sent for it, verbatim
}

// undeliveredInputs are run's inputs, oldest first, that no message carried: each carrier failed or
// expired, and none may still arrive. A carrier whose command row is gone counts as delivered.
func undeliveredInputs(ctx context.Context, tx pgx.Tx, runID string, now time.Time) ([]input, error) {
	rows, err := tx.Query(ctx, `SELECT e.id, e.at, e.kind, coalesce(c0.args->>'text', e.text)
 FROM workflow_run_input_deliveries o
 JOIN workflow_run_events e ON e.id=o.event_id
 LEFT JOIN commands c0 ON c0.id=o.command_id
 WHERE o.run_id=$1 AND o.via='original' AND NOT EXISTS (
  SELECT 1 FROM workflow_run_input_deliveries d LEFT JOIN commands c ON c.id=d.command_id
  WHERE d.event_id=o.event_id AND (d.command_id IS NULL OR c.status='succeeded'
   OR (c.status IN ('queued','sent','running') AND c.expires_at>$2)))
 ORDER BY e.id`, runID, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (input, error) {
		var in input
		err := row.Scan(&in.Event, &in.At, &in.Kind, &in.Text)
		return in, err
	})
}

func (e *Engine) recordInput(ctx context.Context, tx pgx.Tx, r run, kind string, step *int, eventText, commandID string) error {
	var event int64
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, r.ID, r.Account, e.Now(), kind, step, eventText).Scan(&event); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fleet.NotifySQL, fleet.WorkflowRunTopic(r.ID)); err != nil {
		return err
	}
	return e.carried(ctx, tx, r, []input{{Event: event}}, "original", &commandID)
}

// carried records that ins went to the orchestrator via command (nil: an ask's answer).
func (e *Engine) carried(ctx context.Context, tx pgx.Tx, r run, ins []input, via string, command *string) error {
	for _, in := range ins {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_input_deliveries (run_id, account_id, event_id, via, command_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, r.ID, r.Account, in.Event, via, command, e.Now()); err != nil {
			return err
		}
	}
	return nil
}

// redelivery is what the orchestrator reads before anything else in a message carrying ins again.
func redelivery(ins []input) string {
	if len(ins) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Zach's input below may not have reached you: it was recorded while your host could not deliver it, and its message failed or timed out. Here it is verbatim, oldest first. Act on it before anything else, but check first: if you already did what it asks (a timed-out message may still have arrived), do not do it again.\n\n")
	for _, in := range ins {
		at := in.At.UTC().Format(time.RFC3339)
		if in.Kind == "reply" {
			fmt.Fprintf(&b, "--- Zach's reply to your note (your open ask), recorded %s. It answers that question. Do not ask it again. ---\n", at)
		} else {
			fmt.Fprintf(&b, "--- More input for this run, recorded %s ---\n", at)
		}
		b.WriteString(strings.TrimSpace(in.Text))
		b.WriteString("\n--- end ---\n\n")
	}
	return b.String()
}

func withRedelivery(ins []input, text string) string {
	if len(ins) == 0 {
		return text
	}
	return redelivery(ins) + "Now the message for you:\n\n" + text
}

func answeredAsk(ins []input) bool {
	for _, in := range ins {
		if in.Kind == "reply" {
			return true
		}
	}
	return false
}

const answeredAskMessage = "Not asked: Zach already answered your question, and his reply may not have reached you. Act on it; the run is still running. Do not end your turn: carry on in this turn as his reply says.\n\n"
