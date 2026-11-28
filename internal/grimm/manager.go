package grimm

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/attachment"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

const (
	// managerRetryBase is the initial backoff after a failed manager start before the supervisor retries.
	managerRetryBase = 5 * time.Second
	// managerRetryMax caps exponential backoff between automatic manager start retries.
	managerRetryMax = 5 * time.Minute
)

// managerRetryDelay returns exponential backoff (5s, 10s, 20s, ..., capped at 5m) after consecutiveFailures.
func managerRetryDelay(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 0
	}
	d := managerRetryBase
	for i := 1; i < consecutiveFailures && d < managerRetryMax; i++ {
		d *= 2
	}
	if d > managerRetryMax {
		d = managerRetryMax
	}
	return d
}

// postStartFailureNotice appends a thread notice for a manager start failure unless the latest
// thread notice/human row is already the exact same failure notice (preventing retry spam).
func (s *Service) postStartFailureNotice(ctx context.Context, tx pgx.Tx, sess Session, body string) error {
	var lastKind, lastBody string
	err := tx.QueryRow(ctx, `SELECT kind, body FROM grimm_messages
		WHERE session_id = $1 AND channel = 'thread' AND kind IN ('notice', 'human')
		ORDER BY id DESC LIMIT 1`, sess.ID).Scan(&lastKind, &lastBody)
	if err == nil && lastKind == "notice" && lastBody == body {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = s.threadRow(ctx, tx, sess, "notice", "intelligence", body, map[string]string{"level": "error"})
	return err
}

// manager is the newest grimm_managers row: an ended or failed one means the next message starts another.
type manager struct {
	Epoch   int
	Host    string
	AgentID string
	Command *string
	Status  string
}

func currentManager(ctx context.Context, tx pgx.Tx, id string) (manager, bool, error) {
	var m manager
	err := tx.QueryRow(ctx, `SELECT epoch, host, agent_id, command_id::text, status FROM grimm_managers WHERE session_id = $1
		ORDER BY epoch DESC LIMIT 1`, id).Scan(&m.Epoch, &m.Host, &m.AgentID, &m.Command, &m.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

func (m manager) running() bool { return m.Status == "starting" || m.Status == "live" }

// SendToManager adds a human message to the thread and sends it to the manager, which folds it
// into a running turn (steer) or starts one; a session with no manager yet starts one with it.
func (s *Service) SendToManager(ctx context.Context, tx pgx.Tx, sess Session, text string) (int64, error) {
	id, err := s.threadRow(ctx, tx, sess, "human", "human", text, map[string]int{"attachments": len(attachment.Reference.FindAllString(text, -1))})
	if err != nil {
		return 0, err
	}
	if err := s.deliverHuman(ctx, tx, sess, true); err != nil {
		return 0, err
	}
	return id, notify(ctx, tx, sess.ID)
}

// deliverHuman sends the thread's undelivered human messages to the manager, starting one when
// there is none and the session's worktree is ready. When immediate is false (supervisor pass),
// repeated manager start failures back off exponentially so a broken provider cannot cause a retry storm.
func (s *Service) deliverHuman(ctx context.Context, tx pgx.Tx, sess Session, immediate bool) error {
	rows, err := tx.Query(ctx, `SELECT id, body FROM grimm_messages WHERE session_id = $1 AND kind = 'human' AND delivered_at IS NULL ORDER BY id`, sess.ID)
	if err != nil {
		return err
	}
	type msg struct {
		id   int64
		body string
	}
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (msg, error) {
		var m msg
		return m, r.Scan(&m.id, &m.body)
	})
	if err != nil || len(pending) == 0 {
		return err
	}
	m, ok, err := currentManager(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	if ok && m.Status == "starting" {
		return nil // goes once it runs
	}
	if ok && m.Status == "live" && m.AgentID != "" {
		for _, p := range pending {
			c, err := enqueue(ctx, tx, sess.Host, hostclient.SendMessageKind, hostclient.AgentArgs{AgentID: m.AgentID, Text: p.body, Behavior: hostclient.Steer}, commandTTL, s.now())
			if err != nil {
				return s.undeliverable(ctx, tx, sess, p.id, err)
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_messages SET delivered_at = $2, command_id = $3 WHERE id = $1`, p.id, s.now(), c.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if sess.WorkspaceID == "" {
		return nil // the worktree is not ready: the supervisor starts the manager when it is
	}
	if !immediate && ok && m.Status == "failed" {
		var failCount int
		var lastFailed *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), max(coalesce(ended_at, created_at))
			FROM grimm_managers
			WHERE session_id = $1 AND status = 'failed'
			  AND epoch > coalesce((SELECT max(epoch) FROM grimm_managers WHERE session_id = $1 AND status <> 'failed'), 0)
			  AND coalesce(ended_at, created_at) >= coalesce((SELECT max(created_at) FROM grimm_messages WHERE session_id = $1 AND kind = 'human'), 'epoch'::timestamptz)`,
			sess.ID).Scan(&failCount, &lastFailed); err != nil {
			return err
		}
		if failCount > 0 && lastFailed != nil && s.now().Sub(*lastFailed) < managerRetryDelay(failCount) {
			return nil
		}
	}
	texts := make([]string, len(pending))
	for i, p := range pending {
		texts[i] = p.body
	}
	command, err := s.startManager(ctx, tx, sess, texts)
	if err != nil || command == "" {
		return err
	}
	for _, p := range pending {
		if _, err := tx.Exec(ctx, `UPDATE grimm_messages SET delivered_at = $2, command_id = $3 WHERE id = $1`, p.id, s.now(), command); err != nil {
			return err
		}
	}
	return nil
}

// undeliverable marks a human message that cannot go (a credential in it, a missing attachment),
// so it does not block the ones after it, and says why in the thread.
func (s *Service) undeliverable(ctx context.Context, tx pgx.Tx, sess Session, id int64, cause error) error {
	var e *Error
	if !errors.As(cause, &e) || e.Status != 400 {
		return cause
	}
	if _, err := tx.Exec(ctx, `UPDATE grimm_messages SET delivered_at = $2, data = data || '{"undelivered": true}' WHERE id = $1`, id, s.now()); err != nil {
		return err
	}
	_, err := threadRow(ctx, tx, sess, "notice", "intelligence", "That message did not go to Grimm: "+e.Msg, map[string]string{"level": "error"})
	return err
}

// startManager starts the session's next manager with first as its first message, on the plan
// the balancer picks. It returns the create_agent command, or "" when no plan could take it (the
// thread says why, and the next message tries again).
func (s *Service) startManager(ctx context.Context, tx pgx.Tx, sess Session, first []string) (string, error) {
	m, _, err := currentManager(ctx, tx, sess.ID)
	if err != nil {
		return "", err
	}
	epoch := m.Epoch + 1
	c, err := pick(ctx, tx, sess, "manager", "")
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grimm_managers (session_id, account_id, epoch, host, status, error, created_at, ended_at)
			VALUES ($1, $2, $3, $4, 'failed', $5, $6, $6)`, sess.ID, sess.Account, epoch, sess.Host, e.Msg, s.now()); err != nil {
			return "", err
		}
		if err := s.postStartFailureNotice(ctx, tx, sess, "Could not start Grimm: "+e.Msg+". Your next message tries again."); err != nil {
			return "", err
		}
		return "", event(ctx, tx, sess, "intelligence", "manager_failed", "warn", "Could not start the manager: "+e.Msg, nil)
	}
	directives, err := activeDirectives(ctx, tx, sess.ID)
	if err != nil {
		return "", err
	}
	texts := make([]string, len(directives))
	for i, d := range directives {
		texts[i] = d.Text
	}
	cmd, err := enqueue(ctx, tx, sess.Host, hostclient.CreateAgentKind, hostclient.CreateAgentArgs{
		WorkspaceID: sess.WorkspaceID, Provider: c.Provider, Model: c.Model, Thinking: c.Thinking, Mode: c.Mode(),
		Title: "Grimm manager", Prompt: managerPrompt(sess, texts, first),
		Labels: map[string]string{SessionLabel: sess.ID, RoleLabel: "manager", NameLabel: ManagerName, LiveLabel: hostclient.LiveFast},
	}, commandTTL, s.now())
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO grimm_managers (session_id, account_id, epoch, host, command_id, provider, model, thinking, login, long_window, score, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'starting', $12)`, sess.ID, sess.Account, epoch, sess.Host, cmd.ID,
		c.Provider, c.Model, c.Thinking, c.Login, c.LongWindow, c.Score, s.now()); err != nil {
		return "", err
	}
	return cmd.ID, event(ctx, tx, sess, "intelligence", "manager_start", "info",
		"Starting Grimm on "+c.Spec()+" ("+c.Login+")", map[string]any{"epoch": epoch, "candidate": c})
}

// Interrupt cancels the manager's running turn (cancel_agent); the agent and its conversation stay.
func (s *Service) Interrupt(ctx context.Context, account int64, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		return s.interrupt(ctx, tx, sess)
	})
}

func (s *Service) interrupt(ctx context.Context, tx pgx.Tx, sess Session) error {
	m, ok, err := currentManager(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	if !ok || m.Status != "live" || m.AgentID == "" {
		return conflict("Grimm is not running")
	}
	if _, err := enqueue(ctx, tx, sess.Host, hostclient.CancelAgentKind, hostclient.AgentArgs{AgentID: m.AgentID}, commandTTL, s.now()); err != nil {
		return err
	}
	if _, err := threadRow(ctx, tx, sess, "notice", "intelligence", "Interrupted Grimm's turn.", nil); err != nil {
		return err
	}
	if err := event(ctx, tx, sess, "human", "interrupt", "info", "Interrupted the manager", nil); err != nil {
		return err
	}
	return notify(ctx, tx, sess.ID)
}

// NewManager ends the manager's conversation: the next message starts a fresh one. The thread keeps its history.
func (s *Service) NewManager(ctx context.Context, account int64, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		return s.newManager(ctx, tx, sess)
	})
}

func (s *Service) newManager(ctx context.Context, tx pgx.Tx, sess Session) error {
	if err := s.endManager(ctx, tx, sess, false); err != nil {
		return err
	}
	if _, err := threadRow(ctx, tx, sess, "divider", "intelligence", "Fresh manager conversation", nil); err != nil {
		return err
	}
	if err := event(ctx, tx, sess, "human", "new_manager", "info", "Started a fresh manager conversation", nil); err != nil {
		return err
	}
	return notify(ctx, tx, sess.ID)
}

// endManager archives the current manager agent, if any. When stopFirst is true, it also cancels
// any active turn before archiving.
func (s *Service) endManager(ctx context.Context, tx pgx.Tx, sess Session, stopFirst bool) error {
	m, ok, err := currentManager(ctx, tx, sess.ID)
	if err != nil || !ok || !m.running() {
		return err
	}
	if m.AgentID != "" {
		if stopFirst {
			if _, err := enqueue(ctx, tx, sess.Host, hostclient.CancelAgentKind, hostclient.AgentArgs{AgentID: m.AgentID}, commandTTL, s.now()); err != nil {
				return err
			}
		}
		if _, err := enqueue(ctx, tx, sess.Host, hostclient.ArchiveAgentKind, hostclient.AgentArgs{AgentID: m.AgentID}, commandTTL, s.now()); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE grimm_managers SET status = 'ended', ended_at = $3 WHERE session_id = $1 AND epoch = $2`, sess.ID, m.Epoch, s.now())
	return err
}
