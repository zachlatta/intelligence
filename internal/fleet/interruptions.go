package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/interrupt"
)

// interruptionHorizon: a failure older than this when its transcript arrives is history (a first
// sync sends the whole timeline), not an agent waiting to be resumed.
const interruptionHorizon = 6 * time.Hour

// maxFailureItem bounds the transcript items worth parsing: a failure is at most 700 characters of
// text (interrupt.Classify), escaped, in a small message object.
const maxFailureItem = 8 << 10

// ErrNotYours refuses a login incident that is not on one of the account's hosts (or does not exist).
var ErrNotYours = errors.New("no such login incident on your hosts")

// recordInterruptions records each entry of a transcript batch in which the provider refused the
// host's login (internal/interrupt), and marks that login broken. It runs in the batch's
// transaction, so a resend records nothing twice.
func recordInterruptions(ctx context.Context, tx pgx.Tx, host, agentID string, entries []hostclient.TranscriptEntry, at time.Time) error {
	for _, e := range entries {
		// a provider's failure is a short message: skip big items (tool output) without parsing them
		if e.At.IsZero() || at.Sub(e.At) > interruptionHorizon || e.At.Sub(at) > time.Hour || len(e.Item) > maxFailureItem {
			continue
		}
		var item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Item, &item) != nil {
			continue
		}
		reason, ok := interrupt.Classify(e.Provider, item.Type, item.Text)
		if !ok {
			continue
		}
		if err := recordInterruption(ctx, tx, host, agentID, e, reason, at); err != nil {
			return err
		}
	}
	return nil
}

func recordInterruption(ctx context.Context, tx pgx.Tx, host, agentID string, e hostclient.TranscriptEntry, reason interrupt.Reason, at time.Time) error {
	var model, title, parent string
	err := tx.QueryRow(ctx, "SELECT model, title, parent_id FROM agents WHERE host = $1 AND id = $2", host, agentID).Scan(&model, &title, &parent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO agent_interruptions (host, agent_id, seq, at, provider, model, title, parent_id, reason, message, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (host, agent_id, seq) DO NOTHING`,
		host, agentID, e.SeqStart, e.At, e.Provider, model, title, parent, reason.Code, reason.Message, at)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	// the login the host's tool was using when the turn failed (cswap switches them)
	var login string
	err = tx.QueryRow(ctx, `SELECT account FROM usage_active WHERE host = $1 AND provider = $2 AND since <= $3
		ORDER BY since DESC LIMIT 1`, host, e.Provider, e.At).Scan(&login)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	incident, err := openIncident(ctx, tx, host, e.Provider, login, reason, "transcript", e.At)
	if err != nil {
		return err
	}
	var run *string
	err = tx.QueryRow(ctx, `WITH RECURSIVE up AS (
		SELECT id, parent_id, workspace_id FROM agents WHERE host = $1 AND id = $2
		UNION
		SELECT a.id, a.parent_id, a.workspace_id FROM agents a JOIN up ON a.host = $1 AND a.id = up.parent_id)
		SELECT r.id::text FROM workflow_runs r WHERE r.host = $1 AND r.status NOT IN ('succeeded', 'failed', 'canceled')
		AND (r.orchestrator_id = $2 OR r.orchestrator_id IN (SELECT id FROM up)
		     OR r.workspace_id IN (SELECT workspace_id FROM up WHERE workspace_id <> ''))
		ORDER BY r.created_at DESC LIMIT 1`, host, agentID).Scan(&run)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_interruptions SET incident_id = $4, run_id = $5::uuid
		WHERE host = $1 AND agent_id = $2 AND seq = $3`, host, agentID, e.SeqStart, incident, run); err != nil {
		return err
	}
	if run == nil {
		return nil
	}
	who := title
	if who == "" {
		who = "agent " + agentID
	}
	on := e.Provider
	if login != "" {
		on += " login " + login
	}
	text := fmt.Sprintf("Interrupted: %s could not run its turn: %s (%s on %s). Intelligence resumes it once a working login is active.",
		who, reason.Summary, on, host)
	_, err = tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text)
		SELECT id, account_id, $2, 'interrupted', step, $3 FROM workflow_runs WHERE id = $1`, *run, at, text)
	return err
}

// openIncident marks the host's login broken as of at, or extends the incident already open. A
// failure from before the login was last fixed belongs to that fixed incident.
func openIncident(ctx context.Context, tx pgx.Tx, host, provider, login string, reason interrupt.Reason, source string, at time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM login_incidents WHERE host = $1 AND provider = $2 AND login = $3
		AND resolved_at IS NOT NULL AND resolved_at >= $4 AND opened_at <= $4 ORDER BY resolved_at DESC LIMIT 1`,
		host, provider, login, at).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO login_incidents (host, provider, login, reason, message, source, opened_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (host, provider, login) WHERE resolved_at IS NULL DO UPDATE SET
		opened_at = least(login_incidents.opened_at, EXCLUDED.opened_at),
		last_seen_at = greatest(login_incidents.last_seen_at, EXCLUDED.last_seen_at)
		RETURNING id`, host, provider, login, reason.Code, reason.Message, source, at).Scan(&id)
	return id, err
}

// noteLoginHealth follows a login's meter read on host: a read that says the sign-in is gone opens
// (or extends) its incident; a working read taken after an incident opened closes it, and closes
// the host's unknown-login incident too when the login is the one the host uses.
func noteLoginHealth(ctx context.Context, tx pgx.Tx, host, provider, login, readErr string, active bool, observed time.Time) error {
	if readErr != "" {
		reason, broken := interrupt.MeterStatus(provider, readErr)
		if !broken {
			return nil
		}
		_, err := openIncident(ctx, tx, host, provider, login, reason, "meter", observed)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE login_incidents SET resolved_at = $5, resolved_by = 'reading'
		WHERE host = $1 AND provider = $2 AND (login = $3 OR ($4::boolean AND login = '')) AND resolved_at IS NULL AND opened_at < $5`,
		host, provider, login, active, observed)
	return err
}

// ResolveLoginIncident marks a broken login fixed by hand, for account (whose host it must be).
// Resolving one already resolved changes nothing.
func (r *Registry) ResolveLoginIncident(ctx context.Context, account, id int64, note string, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		var resolved bool
		err := tx.QueryRow(ctx, `SELECT i.resolved_at IS NOT NULL FROM login_incidents i
			JOIN host_keys k ON k.host = i.host AND k.account_id = $2 WHERE i.id = $1 FOR UPDATE OF i`, id, account).Scan(&resolved)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotYours
		}
		if err != nil || resolved {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE login_incidents SET resolved_at = $2, resolved_by = 'manual', resolved_note = $3 WHERE id = $1`,
			id, at, note)
		return err
	})
}
