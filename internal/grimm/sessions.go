package grimm

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

// SeedDirective is every new session's first standing rule.
const SeedDirective = "Never push, deploy, publish, open pull requests or send external messages."

// seedAuthor is who the seed directive is added by; only the human can remove such a rule.
const seedAuthor = "intelligence"

// NewSession is what a session is made with.
type NewSession struct {
	Host       string `json:"host"`
	Repo       string `json:"repo"`
	Title      string `json:"title"`
	Base       string `json:"base"`
	MaxWorkers int    `json:"maxWorkers"`
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// CreateSession starts a session for account on a connected host of its: a new git worktree of
// the repository (on its own branch), where the manager starts on the first message.
func (s *Service) CreateSession(ctx context.Context, account int64, n NewSession) (string, error) {
	n.Host, n.Repo, n.Title, n.Base = strings.TrimSpace(n.Host), strings.TrimSpace(n.Repo), strings.TrimSpace(n.Title), strings.TrimSpace(n.Base)
	if n.MaxWorkers == 0 {
		n.MaxWorkers = DefaultMaxWorkers
	}
	if n.MaxWorkers < 1 || n.MaxWorkers > MaxWorkersLimit {
		return "", badRequest("the worker cap must be 1 to %d", MaxWorkersLimit)
	}
	if !hostclient.ValidName(n.Host) {
		return "", badRequest("pick a host")
	}
	if !path.IsAbs(n.Repo) || path.Clean(n.Repo) != n.Repo || len(n.Repo) > 1024 || n.Repo == "/" {
		return "", badRequest("the repository must be a clean absolute directory on the host")
	}
	if path.Base(n.Repo) == "desk" {
		return "", badRequest("Grimm does not run in the desk repository")
	}
	if len(n.Title) > 200 {
		return "", badRequest("the title is longer than 200 bytes")
	}
	if n.Base != "" && (!branchRe.MatchString(n.Base) || strings.Contains(n.Base, "..")) {
		return "", badRequest("the base branch is not a branch name")
	}
	var id string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var online bool
		err := tx.QueryRow(ctx, `SELECT coalesce(h.online, false) FROM host_keys k LEFT JOIN host_status h ON h.name = k.host
			WHERE k.account_id = $1 AND k.host = $2`, account, n.Host).Scan(&online)
		if errors.Is(err, pgx.ErrNoRows) {
			return badRequest("%s is not one of your hosts", n.Host)
		}
		if err != nil {
			return err
		}
		if !online {
			return conflict("%s is not connected", n.Host)
		}
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return err
		}
		sess := Session{ID: id, Account: account, Host: n.Host, Repo: n.Repo, Title: n.Title, Branch: "grimm/" + id[:8], MaxWorkers: n.MaxWorkers, Status: "active"}
		if sess.Title == "" {
			sess.Title = path.Base(n.Repo)
		}
		title := "Grimm: " + sess.Title
		c, err := enqueue(ctx, tx, n.Host, hostclient.CreateWorkspaceKind, hostclient.CreateWorkspaceArgs{
			Cwd: n.Repo, Worktree: true, Branch: sess.Branch, Base: n.Base, Title: cut(title, 120)}, commandTTL, s.now())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grimm_sessions (id, account_id, host, repo, title, branch, base, max_workers, workspace_command_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, id, account, n.Host, n.Repo, sess.Title, sess.Branch, n.Base, n.MaxWorkers, c.ID, s.now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grimm_directives (session_id, account_id, seq, text, added_by) VALUES ($1, $2, 1, $3, $4)`,
			id, account, SeedDirective, seedAuthor); err != nil {
			return err
		}
		if err := event(ctx, tx, sess, "human", "session", "info", "Started a session on "+n.Host+" in "+n.Repo+" (branch "+sess.Branch+")", n); err != nil {
			return err
		}
		return notify(ctx, tx, id)
	})
	return id, err
}

// SessionUpdate changes a session: its worker cap and title.
type SessionUpdate struct {
	MaxWorkers *int    `json:"maxWorkers"`
	Title      *string `json:"title"`
}

func (s *Service) UpdateSession(ctx context.Context, account int64, id string, u SessionUpdate) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if u.MaxWorkers != nil {
			if *u.MaxWorkers < 1 || *u.MaxWorkers > MaxWorkersLimit {
				return badRequest("the worker cap must be 1 to %d", MaxWorkersLimit)
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET max_workers = $2 WHERE id = $1`, id, *u.MaxWorkers); err != nil {
				return err
			}
			if err := event(ctx, tx, sess, "human", "cap", "info", "Set the worker cap to "+strconv.Itoa(*u.MaxWorkers), map[string]int{"maxWorkers": *u.MaxWorkers}); err != nil {
				return err
			}
		}
		if u.Title != nil {
			t := strings.TrimSpace(*u.Title)
			if t == "" || len(t) > 200 {
				return badRequest("a title is 1 to 200 bytes")
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET title = $2 WHERE id = $1`, id, t); err != nil {
				return err
			}
		}
		return notify(ctx, tx, id)
	})
}

// ArchiveSession archives every agent of the session and ends it. The worktree and its files stay.
func (s *Service) ArchiveSession(ctx context.Context, account int64, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if _, err := s.archiveWorkers(ctx, tx, sess, "", "human"); err != nil {
			return err
		}
		if err := s.endManager(ctx, tx, sess); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET status = 'archived', archived_at = $2 WHERE id = $1`, id, s.now()); err != nil {
			return err
		}
		if err := event(ctx, tx, sess, "human", "archive_session", "info", "Archived the session", nil); err != nil {
			return err
		}
		return notify(ctx, tx, id)
	})
}
