// Package grimm runs Grimm sessions: one manager agent (Claude Opus 5.5) per session is the main
// thread, and it hires named workers on the account's plans, which talk to each other and to it on
// a shared bus. Every agent is a Paseo agent on the session's host, started and steered through the
// host command queue (fleet.EnqueueTx), so nothing here holds a process or a login. All state is in
// the grimm_* tables (readable with read-only SQL); the Supervisor moves sessions along as hosts
// report, and survives a restart because it keeps nothing in memory.
package grimm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

const (
	// ManagerName is how workers and the bus address the manager.
	ManagerName = "grimm"
	// DefaultMaxWorkers is a new session's live-worker cap; each session can set its own.
	DefaultMaxWorkers = 20
	// MaxWorkersLimit is the highest cap a session can set.
	MaxWorkersLimit = 100
	// ManagerProvider and ManagerModel are what the manager runs on.
	ManagerProvider = "claude"
	ManagerModel    = "claude-opus-5-5"
	// MaxText bounds one message, task or directive.
	MaxText = 20000
	// commandTTL is how long a Grimm command may wait for its host.
	commandTTL = 10 * time.Minute
	// updateDebounce gathers worker news for this long before waking the manager.
	updateDebounce = 2 * time.Second
	// startGrace: an agent just asked to work that still reads idle after this has finished.
	startGrace = 20 * time.Second
)

// Labels on every Grimm agent. LiveLabel asks the host to copy its transcript at once while it
// runs, so the page streams it with little delay.
const (
	SessionLabel = "intelligence.grimm.session"
	RoleLabel    = "intelligence.grimm.role"
	NameLabel    = "intelligence.grimm.name"
	LiveLabel    = hostclient.LiveLabel
)

// Service is Grimm's writes. DB is the owner pool; reads for an account go through its own role.
type Service struct {
	DB  *pgxpool.Pool
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Error is a refusal a person or agent can act on, with the HTTP status it answers with.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func refuse(status int, format string, a ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) error { return refuse(400, format, a...) }
func conflict(format string, a ...any) error   { return refuse(409, format, a...) }

// ErrNotFound: no such session (or worker) in the account.
var ErrNotFound = &Error{Status: 404, Msg: "no such Grimm session"}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id can name a session.
func ValidID(id string) bool { return uuidRe.MatchString(id) }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Session is a session row with what the code needs of it.
type Session struct {
	ID          string
	Account     int64
	Host        string
	Repo        string
	Title       string
	Branch      string
	WorkspaceID string
	MaxWorkers  int
	Status      string
}

func lockSession(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('grimm:' || $1, 0))`, id)
	return err
}

// session reads and locks a session of account (account 0: any, for the supervisor and agents).
func session(ctx context.Context, tx pgx.Tx, account int64, id string) (Session, error) {
	if !ValidID(id) {
		return Session{}, ErrNotFound
	}
	if err := lockSession(ctx, tx, id); err != nil {
		return Session{}, err
	}
	var s Session
	err := tx.QueryRow(ctx, `SELECT id::text, account_id, host, repo, title, branch, workspace_id, max_workers, status
		FROM grimm_sessions WHERE id = $1 AND ($2 = 0 OR account_id = $2)`, id, account).
		Scan(&s.ID, &s.Account, &s.Host, &s.Repo, &s.Title, &s.Branch, &s.WorkspaceID, &s.MaxWorkers, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

func activeSession(ctx context.Context, tx pgx.Tx, account int64, id string) (Session, error) {
	s, err := session(ctx, tx, account, id)
	if err == nil && s.Status != "active" {
		err = conflict("this session is archived")
	}
	return s, err
}

// notify wakes the session's stream and the supervisor once tx commits.
func notify(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, fleet.NotifySQL, fleet.GrimmTopic(id)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, fleet.NotifySQL, fleet.GrimmTopic(""))
	return err
}

func event(ctx context.Context, tx pgx.Tx, s Session, actor, kind, level, text string, data any) error {
	b, _ := json.Marshal(data)
	if data == nil {
		b = []byte("{}")
	}
	_, err := tx.Exec(ctx, `INSERT INTO grimm_events (session_id, account_id, actor, event, level, text, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.ID, s.Account, actor, kind, level, text, b)
	return err
}

func threadRow(ctx context.Context, tx pgx.Tx, s Session, kind, sender, body string, data any) (int64, error) {
	b, _ := json.Marshal(data)
	if data == nil {
		b = []byte("{}")
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO grimm_messages (session_id, account_id, channel, sender, kind, body, data)
		VALUES ($1, $2, 'thread', $3, $4, $5, $6) RETURNING id`, s.ID, s.Account, sender, kind, body, b).Scan(&id)
	return id, err
}

func busRow(ctx context.Context, tx pgx.Tx, s Session, sender, recipient, kind, body string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body)
		VALUES ($1, $2, 'bus', $3, $4, $5, $6) RETURNING id`, s.ID, s.Account, sender, recipient, kind, body).Scan(&id)
	return id, err
}

func enqueue(ctx context.Context, tx pgx.Tx, host, kind string, args any, ttl time.Duration, at time.Time) (fleet.Command, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return fleet.Command{}, err
	}
	// in a savepoint, so a refused command leaves the rest of tx usable
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fleet.Command{}, err
	}
	c, err := fleet.EnqueueTx(ctx, sp, host, kind, b, ttl, at)
	if err != nil {
		_ = sp.Rollback(ctx)
	} else if err = sp.Commit(ctx); err != nil {
		return c, err
	}
	var bad *fleet.BadCommandError
	if errors.As(err, &bad) {
		return c, badRequest("%v", bad.Err)
	}
	if errors.Is(err, fleet.ErrUnknownHost) {
		return c, conflict("host %s has never connected", host)
	}
	return c, err
}

// cleanText trims a message and checks its size.
func cleanText(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", badRequest("%s is empty", what)
	}
	if len(s) > MaxText {
		return "", badRequest("%s is longer than %d bytes", what, MaxText)
	}
	return s, nil
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8Valid(s) {
		s = s[:len(s)-1]
	}
	return s + " …"
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
