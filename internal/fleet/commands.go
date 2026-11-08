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

	"github.com/zachlatta/intelligence/internal/hostclient"
)

var (
	// ErrUnknownHost refuses a command for a host that has never connected.
	ErrUnknownHost = errors.New("no such host")
	// ErrUnknownCommand: no command with that id for that host.
	ErrUnknownCommand = errors.New("no such command")
)

// BadCommandError refuses a command whose kind or arguments do not parse (hostclient.ParseCommand).
type BadCommandError struct{ Err error }

func (e *BadCommandError) Error() string { return e.Err.Error() }
func (e *BadCommandError) Unwrap() error { return e.Err }

// Command is a command and how far it got (the commands table).
type Command struct {
	ID         string          `json:"id"`
	Host       string          `json:"host"`
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
	return c.Status == hostclient.CommandSucceeded || c.Status == hostclient.CommandFailed || c.Status == hostclient.CommandExpired
}

// Wire is the command as a host receives it.
func (c Command) Wire() hostclient.Command {
	return hostclient.Command{ID: c.ID, Kind: c.Kind, Args: c.Args, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}
}

const commandColumns = `id::text, host, kind, args, status, result, error, attempts, created_at, expires_at, sent_at, started_at, finished_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	var result []byte
	var sent, started, finished *time.Time
	err := row.Scan(&c.ID, &c.Host, &c.Kind, &c.Args, &c.Status, &result, &c.Error, &c.Attempts, &c.CreatedAt, &c.ExpiresAt,
		&sent, &started, &finished)
	if result != nil {
		c.Result = result
	}
	c.CreatedAt, c.ExpiresAt = c.CreatedAt.UTC(), c.ExpiresAt.UTC()
	c.SentAt, c.StartedAt, c.FinishedAt = deref(sent), deref(started), deref(finished)
	return c, err
}

// Enqueue queues a command for host, created at at, that expires ttl later.
func (r *Registry) Enqueue(ctx context.Context, host, kind string, args json.RawMessage, ttl time.Duration, at time.Time) (Command, error) {
	if _, err := hostclient.ParseCommand(kind, args); err != nil {
		return Command{}, &BadCommandError{err}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var known bool
	if err := r.p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM hosts WHERE name = $1)", host).Scan(&known); err != nil {
		return Command{}, err
	}
	if !known {
		return Command{}, ErrUnknownHost
	}
	var c Command
	err := pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		var err error
		c, err = scanCommand(tx.QueryRow(ctx, `INSERT INTO commands (host, kind, args, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+commandColumns, host, kind, []byte(args), at, at.Add(ttl)))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, notifySQL, CommandsTopic(host))
		return err
	})
	return c, err
}

// Command is one command; pgx.ErrNoRows when there is none.
func (r *Registry) Command(ctx context.Context, id string) (Command, error) {
	if !uuidRe.MatchString(id) {
		return Command{}, pgx.ErrNoRows
	}
	return scanCommand(r.p.QueryRow(ctx, "SELECT "+commandColumns+" FROM commands WHERE id = $1", id))
}

// Claim marks host's commands sent and returns them, oldest first, to go down its stream at at.
// Queued ones are claimed; with unfinished (a new stream), so are those sent before with no result
// yet, as the host may never have received them. Commands past their expiry expire instead.
func (r *Registry) Claim(ctx context.Context, host string, unfinished bool, at time.Time) ([]Command, error) {
	if _, err := r.p.Exec(ctx, `WITH expired AS (UPDATE commands SET status = 'expired', finished_at = $2,
		error = CASE status WHEN 'queued' THEN 'expired before the host was connected to receive it'
		                    ELSE 'expired with no result from the host: it may or may not have run' END
		WHERE host = $1 AND status IN ('queued', 'sent', 'running') AND expires_at <= $2 RETURNING id)
		SELECT pg_notify('`+channel+`', 'command:' || id) FROM expired`, host, at); err != nil {
		return nil, err
	}
	statuses := []string{hostclient.CommandQueued}
	if unfinished {
		statuses = append(statuses, hostclient.CommandSent, hostclient.CommandRunning)
	}
	rows, err := r.p.Query(ctx, `UPDATE commands SET status = CASE WHEN status = 'queued' THEN 'sent' ELSE status END,
		sent_at = coalesce(sent_at, $2), attempts = attempts + 1
		WHERE id IN (SELECT id FROM commands WHERE host = $1 AND status = ANY($3) AND expires_at > $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+commandColumns, host, at, statuses)
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

// UpdateCommand records a host's update to one of its commands, received at at. The first result
// stands: a later update (a resent command reporting again) changes nothing. A result for an
// expired command replaces the expiry, as it is what happened.
func (r *Registry) UpdateCommand(ctx context.Context, host string, u hostclient.CommandUpdate, at time.Time) error {
	if !uuidRe.MatchString(u.ID) {
		return ErrUnknownCommand
	}
	var sql string
	args := []any{host, u.ID, at}
	switch u.Status {
	case hostclient.CommandRunning:
		sql = `UPDATE commands SET status = 'running', started_at = coalesce(started_at, $3)
			WHERE host = $1 AND id = $2 AND status IN ('queued', 'sent', 'running')`
	case hostclient.CommandSucceeded, hostclient.CommandFailed:
		var result []byte
		if len(u.Result) > 0 {
			if !json.Valid(u.Result) {
				return errors.New("command result is not JSON")
			}
			result = u.Result
		}
		sql = `UPDATE commands SET status = $4, result = $5, error = $6, finished_at = $3
			WHERE host = $1 AND id = $2 AND status NOT IN ('succeeded', 'failed')`
		args = append(args, u.Status, result, u.Error)
	default:
		return fmt.Errorf("a host cannot set a command %q", u.Status)
	}
	var changed bool
	err := pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		changed = true
		_, err = tx.Exec(ctx, notifySQL, CommandTopic(u.ID))
		return err
	})
	if err != nil || changed {
		return err
	}
	var known bool
	if err := r.p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM commands WHERE host = $1 AND id = $2)", host, u.ID).Scan(&known); err != nil {
		return err
	}
	if !known {
		return ErrUnknownCommand
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
