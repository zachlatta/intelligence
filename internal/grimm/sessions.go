package grimm

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
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

// DeskName is the repository a session starts in when none is named: desk, the general
// workspace, where answer agents also run.
const DeskName = "desk"

// DeskRepo is host's desk checkout, or "" when it has none: a git directory in host_repositories
// (the registered-project and workspace catalog answer agents use) named desk by its project or
// its directory's base name, case-insensitively.
func DeskRepo(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, host string) (string, error) {
	var dir string
	err := q.QueryRow(ctx, `SELECT directory FROM host_repositories
		WHERE host = $1 AND directory LIKE '/%' AND (lower(regexp_replace(rtrim(directory, '/'), '^.*/', '')) = $2 OR lower(trim(project)) = $2)
		ORDER BY lower(regexp_replace(rtrim(directory, '/'), '^.*/', '')) = $2 DESC, directory LIMIT 1`, host, DeskName).Scan(&dir)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path.Clean(dir), nil
}

// CreateSession starts a session for account on a connected host of its: a new git worktree of
// the repository (desk when none is named) on its own branch, where the manager starts on the
// first message. A session never works in a main checkout, desk's included.
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
	if n.Repo != "" && (!path.IsAbs(n.Repo) || path.Clean(n.Repo) != n.Repo || len(n.Repo) > 1024 || n.Repo == "/") {
		return "", badRequest("the repository must be a clean absolute directory on the host")
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
		if n.Repo == "" {
			if n.Repo, err = DeskRepo(ctx, tx, n.Host); err != nil {
				return err
			}
			if n.Repo == "" {
				return badRequest("%s has no desk repository registered: name a repository", n.Host)
			}
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
			if err := s.setMaxWorkers(ctx, tx, sess, *u.MaxWorkers); err != nil {
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

// setMaxWorkers updates the session's live-worker cap and notifies a running or starting manager
// when the cap actually changed, so it does not stay stuck on the cap baked into its first prompt.
func (s *Service) setMaxWorkers(ctx context.Context, tx pgx.Tx, sess Session, n int) error {
	if n < 1 || n > MaxWorkersLimit {
		return badRequest("the worker cap must be 1 to %d", MaxWorkersLimit)
	}
	if n == sess.MaxWorkers {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET max_workers = $2 WHERE id = $1`, sess.ID, n); err != nil {
		return err
	}
	if err := event(ctx, tx, sess, "human", "cap", "info", fmt.Sprintf("Set the worker cap to %d", n), map[string]int{"maxWorkers": n}); err != nil {
		return err
	}
	m, ok, err := currentManager(ctx, tx, sess.ID)
	if err != nil || !ok {
		return err
	}
	summary := fmt.Sprintf("worker cap set to %d", n)
	detail := fmt.Sprintf("The session's live-worker cap changed from %d to %d. You may now have up to %d live workers.", sess.MaxWorkers, n, n)
	if m.Status == "live" && m.AgentID != "" {
		text := updateText([]string{detail})
		c, err := enqueue(ctx, tx, sess.Host, hostclient.SendMessageKind, hostclient.AgentArgs{AgentID: m.AgentID, Text: text, Behavior: hostclient.Steer}, commandTTL, s.now())
		if err != nil {
			return err
		}
		_, err = threadRow(ctx, tx, sess, "update", "intelligence", text, map[string]any{"summaries": []string{summary}, "commandId": c.ID})
		return err
	}
	if m.Status == "starting" {
		return s.wake(ctx, tx, sess, "", summary, detail)
	}
	return nil
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
