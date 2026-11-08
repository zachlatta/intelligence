package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/devbox"
)

var (
	// ErrUnknownBox refuses a command for a box that has never reported.
	ErrUnknownBox = errors.New("no such dev box")
	// ErrUnknownCommand: no command with that id for that box.
	ErrUnknownCommand = errors.New("no such command")
)

// BadCommandError refuses a command whose kind or arguments do not parse (devbox.ParseCommand).
type BadCommandError struct{ Err error }

func (e *BadCommandError) Error() string { return e.Err.Error() }
func (e *BadCommandError) Unwrap() error { return e.Err }

// Command is a command and how far it got (the commands table).
type Command struct {
	ID         string          `json:"id"`
	Box        string          `json:"box"`
	Kind       string          `json:"kind"`
	Args       json.RawMessage `json:"args"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Attempts   int             `json:"attempts"`
	CreatedAt  time.Time       `json:"createdAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	SentAt     time.Time       `json:"sentAt,omitzero"`
	StartedAt  time.Time       `json:"startedAt,omitzero"`
	FinishedAt time.Time       `json:"finishedAt,omitzero"`
}

// Finished reports whether c will not change again (expired can, if a late result arrives).
func (c Command) Finished() bool {
	return c.Status == devbox.CommandSucceeded || c.Status == devbox.CommandFailed || c.Status == devbox.CommandExpired
}

// Wire is the command as a box receives it.
func (c Command) Wire() devbox.Command {
	return devbox.Command{ID: c.ID, Kind: c.Kind, Args: c.Args, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}
}

const commandColumns = `id::text, box, kind, args, status, result, error, attempts, created_at, expires_at, sent_at, started_at, finished_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	var result []byte
	var sent, started, finished *time.Time
	err := row.Scan(&c.ID, &c.Box, &c.Kind, &c.Args, &c.Status, &result, &c.Error, &c.Attempts, &c.CreatedAt, &c.ExpiresAt,
		&sent, &started, &finished)
	if result != nil {
		c.Result = result
	}
	c.CreatedAt, c.ExpiresAt = c.CreatedAt.UTC(), c.ExpiresAt.UTC()
	c.SentAt, c.StartedAt, c.FinishedAt = deref(sent), deref(started), deref(finished)
	return c, err
}

// Enqueue queues a command for box, created at at, that expires ttl later.
func (r *Registry) Enqueue(ctx context.Context, box, kind string, args json.RawMessage, ttl time.Duration, at time.Time) (Command, error) {
	if _, err := devbox.ParseCommand(kind, args); err != nil {
		return Command{}, &BadCommandError{err}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var known bool
	if err := r.p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM devboxes WHERE name = $1)", box).Scan(&known); err != nil {
		return Command{}, err
	}
	if !known {
		return Command{}, ErrUnknownBox
	}
	return scanCommand(r.p.QueryRow(ctx, `INSERT INTO commands (box, kind, args, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+commandColumns, box, kind, []byte(args), at, at.Add(ttl)))
}

// Command is one command; pgx.ErrNoRows when there is none.
func (r *Registry) Command(ctx context.Context, id string) (Command, error) {
	if !uuidRe.MatchString(id) {
		return Command{}, pgx.ErrNoRows
	}
	return scanCommand(r.p.QueryRow(ctx, "SELECT "+commandColumns+" FROM commands WHERE id = $1", id))
}

// Claim marks box's commands sent and returns them, oldest first, to go down its stream at at.
// Queued ones are claimed; with unfinished (a new stream), so are those sent before with no result
// yet, as the box may never have received them. Commands past their expiry expire instead.
func (r *Registry) Claim(ctx context.Context, box string, unfinished bool, at time.Time) ([]Command, error) {
	if _, err := r.p.Exec(ctx, `UPDATE commands SET status = 'expired', finished_at = $2,
		error = CASE status WHEN 'queued' THEN 'expired before the box was connected to receive it'
		                    ELSE 'expired with no result from the box: it may or may not have run' END
		WHERE box = $1 AND status IN ('queued', 'sent', 'running') AND expires_at <= $2`, box, at); err != nil {
		return nil, err
	}
	statuses := []string{devbox.CommandQueued}
	if unfinished {
		statuses = append(statuses, devbox.CommandSent, devbox.CommandRunning)
	}
	rows, err := r.p.Query(ctx, `UPDATE commands SET status = CASE WHEN status = 'queued' THEN 'sent' ELSE status END,
		sent_at = coalesce(sent_at, $2), attempts = attempts + 1
		WHERE id IN (SELECT id FROM commands WHERE box = $1 AND status = ANY($3) AND expires_at > $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+commandColumns, box, at, statuses)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Command, error) { return scanCommand(row) })
	slices.SortFunc(out, func(a, b Command) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, err
}

// UpdateCommand records a box's update to one of its commands, received at at. The first result
// stands: a later update (a resent command reporting again) changes nothing. A result for an
// expired command replaces the expiry, as it is what happened.
func (r *Registry) UpdateCommand(ctx context.Context, box string, u devbox.CommandUpdate, at time.Time) error {
	if !uuidRe.MatchString(u.ID) {
		return ErrUnknownCommand
	}
	var sql string
	args := []any{box, u.ID, at}
	switch u.Status {
	case devbox.CommandRunning:
		sql = `UPDATE commands SET status = 'running', started_at = coalesce(started_at, $3)
			WHERE box = $1 AND id = $2 AND status IN ('queued', 'sent', 'running')`
	case devbox.CommandSucceeded, devbox.CommandFailed:
		var result []byte
		if len(u.Result) > 0 {
			if !json.Valid(u.Result) {
				return errors.New("command result is not JSON")
			}
			result = u.Result
		}
		sql = `UPDATE commands SET status = $4, result = $5, error = $6, finished_at = $3
			WHERE box = $1 AND id = $2 AND status NOT IN ('succeeded', 'failed')`
		args = append(args, u.Status, result, u.Error)
	default:
		return fmt.Errorf("a box cannot set a command %q", u.Status)
	}
	tag, err := r.p.Exec(ctx, sql, args...)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	var known bool
	if err := r.p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM commands WHERE box = $1 AND id = $2)", box, u.ID).Scan(&known); err != nil {
		return err
	}
	if !known {
		return ErrUnknownCommand
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
