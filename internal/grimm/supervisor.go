package grimm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// The supervisor moves each active session along: it picks up the host's answers to the session's
// commands (the worktree, a manager or a worker started or failed), follows its agents' statuses
// in the hosts' reports, and wakes the manager with what its workers did. Everything it acts on is
// in the tables, so after a restart it carries on where it was. One server instance at a time runs
// it (a session advisory lock in each pass keeps two from racing).

// RunEvery is the supervisor's fallback pass, for the debounce and grace timers.
const RunEvery = time.Second

// Run supervises every active session until ctx ends, waking on Grimm changes and host reports.
func (s *Service) Run(ctx context.Context, changes *fleet.Changes) {
	grimm, stop := changes.Subscribe(fleet.GrimmTopic(""))
	defer stop()
	hostSubs := map[string]func(){}
	defer func() {
		for _, stop := range hostSubs {
			stop()
		}
	}()
	reports := make(chan struct{}, 1)
	tick := time.NewTicker(RunEvery)
	defer tick.Stop()
	lastErr := ""
	for {
		hosts, err := s.Pass(ctx)
		if msg := fmt.Sprint(err); err != nil && msg != lastErr && ctx.Err() == nil {
			log.Printf("grimm supervisor: %v", err)
			lastErr = msg
		} else if err == nil {
			lastErr = ""
		}
		for _, h := range hosts {
			if hostSubs[h] != nil {
				continue
			}
			ch, stop := changes.Subscribe(fleet.AgentsTopic(h))
			done := make(chan struct{})
			go func() {
				for {
					select {
					case <-done:
						return
					case <-ch:
						select {
						case reports <- struct{}{}:
						default:
						}
					}
				}
			}()
			hostSubs[h] = func() { close(done); stop() }
		}
		select {
		case <-ctx.Done():
			return
		case <-grimm:
		case <-reports:
		case <-tick.C:
		}
	}
}

// Pass reconciles every active session once, and returns their hosts.
func (s *Service) Pass(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text, host FROM grimm_sessions WHERE status = 'active' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	type sh struct{ id, host string }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sh, error) {
		var x sh
		return x, r.Scan(&x.id, &x.host)
	})
	if err != nil {
		return nil, err
	}
	var hosts []string
	seen := map[string]bool{}
	var first error
	for _, x := range list {
		if !seen[x.host] {
			seen[x.host] = true
			hosts = append(hosts, x.host)
		}
		if err := s.Reconcile(ctx, x.id); err != nil && first == nil {
			first = fmt.Errorf("session %s: %w", x.id, err)
		}
	}
	return hosts, first
}

// cmdState is a command as the host last reported it; an expired one counts as failed.
type cmdState struct {
	status string
	result hostclient.CommandResult
	err    string
}

func commandState(ctx context.Context, tx pgx.Tx, id *string) (cmdState, error) {
	var c cmdState
	if id == nil {
		return c, nil
	}
	var result []byte
	err := tx.QueryRow(ctx, `SELECT status, result, error FROM commands WHERE id = $1`, *id).Scan(&c.status, &result, &c.err)
	if err != nil {
		return c, err
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &c.result)
	}
	return c, nil
}

func (c cmdState) failed() bool {
	return c.status == hostclient.CommandFailed || c.status == hostclient.CommandExpired
}

// Reconcile moves one session along, in one transaction under its lock.
func (s *Service) Reconcile(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := session(ctx, tx, 0, id)
		if err != nil || sess.Status != "active" {
			return err
		}
		changed := false
		mark := func(ok bool, err error) error {
			changed = changed || ok
			return err
		}
		if err := mark(s.reconcileWorkspace(ctx, tx, &sess)); err != nil {
			return err
		}
		if err := mark(s.reconcileManagers(ctx, tx, sess)); err != nil {
			return err
		}
		if err := mark(s.reconcileWorkers(ctx, tx, sess)); err != nil {
			return err
		}
		if err := s.deliverHuman(ctx, tx, sess); err != nil {
			return err
		}
		if err := mark(s.flushUpdates(ctx, tx, sess)); err != nil {
			return err
		}
		if changed {
			return notify(ctx, tx, sess.ID)
		}
		return nil
	})
}

func (s *Service) reconcileWorkspace(ctx context.Context, tx pgx.Tx, sess *Session) (bool, error) {
	if sess.WorkspaceID != "" {
		return false, nil
	}
	var cmd *string
	var failure string
	if err := tx.QueryRow(ctx, `SELECT workspace_command_id::text, workspace_error FROM grimm_sessions WHERE id = $1`, sess.ID).Scan(&cmd, &failure); err != nil {
		return false, err
	}
	if failure != "" {
		return false, nil
	}
	c, err := commandState(ctx, tx, cmd)
	if err != nil {
		return false, err
	}
	switch {
	case c.status == hostclient.CommandSucceeded && c.result.WorkspaceID != "":
		sess.WorkspaceID = c.result.WorkspaceID
		if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET workspace_id = $2 WHERE id = $1`, sess.ID, sess.WorkspaceID); err != nil {
			return false, err
		}
		return true, event(ctx, tx, *sess, "intelligence", "workspace", "info", "The session's worktree is ready on "+sess.Host+" (branch "+sess.Branch+")", nil)
	case c.failed():
		why := c.err
		if why == "" {
			why = "the host did not answer in time"
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET workspace_error = $2 WHERE id = $1`, sess.ID, why); err != nil {
			return false, err
		}
		if _, err := threadRow(ctx, tx, *sess, "notice", "intelligence", "Could not make the session's worktree on "+sess.Host+": "+why, map[string]string{"level": "error"}); err != nil {
			return false, err
		}
		return true, event(ctx, tx, *sess, "intelligence", "workspace_failed", "warn", "Could not make the worktree: "+why, nil)
	}
	return false, nil
}

func (s *Service) reconcileManagers(ctx context.Context, tx pgx.Tx, sess Session) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT epoch, command_id::text, status FROM grimm_managers WHERE session_id = $1 AND agent_id = ''
		AND command_id IS NOT NULL AND status IN ('starting', 'ended')`, sess.ID)
	if err != nil {
		return false, err
	}
	type pm struct {
		epoch  int
		cmd    *string
		status string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pm, error) {
		var x pm
		return x, r.Scan(&x.epoch, &x.cmd, &x.status)
	})
	if err != nil {
		return false, err
	}
	changed := false
	for _, m := range list {
		c, err := commandState(ctx, tx, m.cmd)
		if err != nil {
			return false, err
		}
		switch {
		case c.status == hostclient.CommandSucceeded && c.result.AgentID != "":
			changed = true
			if m.status == "ended" { // /new or archive came before it started: do not strand it
				if _, err := tx.Exec(ctx, `UPDATE grimm_managers SET agent_id = $3 WHERE session_id = $1 AND epoch = $2`, sess.ID, m.epoch, c.result.AgentID); err != nil {
					return false, err
				}
				if _, err := enqueue(ctx, tx, sess.Host, hostclient.ArchiveAgentKind, hostclient.AgentArgs{AgentID: c.result.AgentID}, commandTTL, s.now()); err != nil {
					return false, err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_managers SET agent_id = $3, status = 'live' WHERE session_id = $1 AND epoch = $2`, sess.ID, m.epoch, c.result.AgentID); err != nil {
				return false, err
			}
			if err := event(ctx, tx, sess, "intelligence", "manager_live", "info", "Grimm is running", map[string]string{"agentId": c.result.AgentID}); err != nil {
				return false, err
			}
		case c.failed() && m.status == "starting":
			changed = true
			why := c.err
			if why == "" {
				why = "the host did not answer in time"
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_managers SET status = 'failed', error = $3, ended_at = $4 WHERE session_id = $1 AND epoch = $2`,
				sess.ID, m.epoch, why, s.now()); err != nil {
				return false, err
			}
			if _, err := threadRow(ctx, tx, sess, "notice", "intelligence", "Grimm could not start: "+why+". Your next message tries again.", map[string]string{"level": "error"}); err != nil {
				return false, err
			}
			if err := event(ctx, tx, sess, "intelligence", "manager_failed", "warn", "The manager failed to start: "+why, nil); err != nil {
				return false, err
			}
		}
	}
	return changed, nil
}

type liveWorker struct {
	name, role, provider, model, status, agentID, agentStatus string
	cmd                                                       *string
	seenRunning, resultPosted, reported, connected            bool
	turnAt                                                    time.Time
}

func (s *Service) reconcileWorkers(ctx context.Context, tx pgx.Tx, sess Session) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT w.name, w.role, w.provider, w.model, w.status, w.agent_id, w.command_id::text, w.seen_running, w.result_posted, w.turn_at,
		coalesce(a.status, ''), a.id IS NOT NULL, coalesce(h.connected, false)
		FROM grimm_workers w LEFT JOIN agents a ON a.host = w.host AND a.id = w.agent_id AND w.agent_id <> ''
		LEFT JOIN hosts h ON h.name = w.host
		WHERE w.session_id = $1 AND (w.status IN ('starting', 'running', 'idle') OR (w.status = 'archived' AND w.agent_id = '' AND w.command_id IS NOT NULL))`, sess.ID)
	if err != nil {
		return false, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (liveWorker, error) {
		var x liveWorker
		return x, r.Scan(&x.name, &x.role, &x.provider, &x.model, &x.status, &x.agentID, &x.cmd, &x.seenRunning, &x.resultPosted, &x.turnAt,
			&x.agentStatus, &x.reported, &x.connected)
	})
	if err != nil {
		return false, err
	}
	changed := false
	for _, w := range list {
		ok, err := s.reconcileWorker(ctx, tx, sess, w)
		if err != nil {
			return false, err
		}
		changed = changed || ok
	}
	return changed, nil
}

func (s *Service) reconcileWorker(ctx context.Context, tx pgx.Tx, sess Session, w liveWorker) (bool, error) {
	set := func(q string, args ...any) error {
		_, err := tx.Exec(ctx, `UPDATE grimm_workers SET `+q+` WHERE session_id = $1 AND name = $2`, append([]any{sess.ID, w.name}, args...)...)
		return err
	}
	if w.agentID == "" {
		c, err := commandState(ctx, tx, w.cmd)
		if err != nil {
			return false, err
		}
		switch {
		case c.status == hostclient.CommandSucceeded && c.result.AgentID != "":
			if w.status == "archived" { // archived while it started: do not strand it
				if _, err := enqueue(ctx, tx, sess.Host, hostclient.ArchiveAgentKind, hostclient.AgentArgs{AgentID: c.result.AgentID}, commandTTL, s.now()); err != nil {
					return false, err
				}
				return true, set(`agent_id = $3`, c.result.AgentID)
			}
			if err := set(`agent_id = $3, status = 'running'`, c.result.AgentID); err != nil {
				return false, err
			}
			return true, event(ctx, tx, sess, "intelligence", "worker_started", "info", w.name+" started on "+w.provider+"/"+w.model, map[string]string{"worker": w.name, "agentId": c.result.AgentID})
		case c.failed() && w.status != "archived":
			why := c.err
			if why == "" {
				why = "the host did not answer in time"
			}
			if err := set(`status = 'failed', finished_at = $3, last_response = $4`, s.now(), "Could not start: "+why); err != nil {
				return false, err
			}
			if err := s.wake(ctx, tx, sess, w.name, w.name+" failed to start", fmt.Sprintf("%s (%s) failed to start: %s", w.name, w.role, cut(why, 500))); err != nil {
				return false, err
			}
			return true, event(ctx, tx, sess, "intelligence", "worker_failed", "warn", w.name+" failed to start: "+cut(why, 200), map[string]string{"worker": w.name})
		}
		return false, nil
	}
	switch {
	case w.agentStatus == "running" || w.agentStatus == "initializing":
		if w.status != "running" || !w.seenRunning {
			return true, set(`status = 'running', seen_running = true`)
		}
	case (w.agentStatus == "idle" || w.agentStatus == "error") && w.status == "running" &&
		(w.seenRunning || s.now().Sub(w.turnAt) > startGrace):
		reply, err := lastReply(ctx, tx, sess.Host, w.agentID)
		if err != nil {
			return false, err
		}
		if err := set(`status = 'idle', finished_at = $3, last_response = $4, seen_running = false`, s.now(), reply); err != nil {
			return false, err
		}
		if err := event(ctx, tx, sess, "intelligence", "worker_finished", "info", fmt.Sprintf("%s (%s/%s) finished: %s", w.name, w.provider, w.model, w.agentStatus),
			map[string]string{"worker": w.name}); err != nil {
			return false, err
		}
		if w.resultPosted {
			return true, set(`result_posted = false`) // it already reported to the manager on the bus
		}
		shown := strings.TrimSpace(reply)
		if shown == "" {
			shown = "(no text reply)"
		}
		if len(shown) > 2500 {
			shown = cut(shown, 2500) + " (truncated; `" + ctlCmd + " result " + w.name + "` has all of it)"
		}
		return true, s.wake(ctx, tx, sess, w.name, w.name+" finished",
			fmt.Sprintf("Worker **%s** (%s, %s/%s) finished its turn (status: %s). Its final reply:\n\n%s", w.name, w.role, w.provider, w.model, w.agentStatus, shown))
	case !w.reported && w.connected && w.status != "archived" && s.now().Sub(w.turnAt) > 3*startGrace:
		// gone from a connected host: archived outside Grimm
		if err := set(`status = 'archived', archived_at = $3`, s.now()); err != nil {
			return false, err
		}
		return true, event(ctx, tx, sess, "intelligence", "worker_gone", "warn", w.name+"'s agent is gone from "+sess.Host+"; marked archived", map[string]string{"worker": w.name})
	}
	return false, nil
}

// lastReply is an agent's newest assistant message, from the server's copy of its transcript (the
// host copies it before reporting the agent idle, so the reply is there when the turn ends).
func lastReply(ctx context.Context, tx pgx.Tx, host, agent string) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `SELECT text FROM transcript_entries WHERE host = $1 AND agent_id = $2 AND kind = 'assistant_message'
		ORDER BY seq_start DESC LIMIT 1`, host, agent).Scan(&text)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return text, err
}

// flushUpdates wakes an idle manager with the session's pending news, once it has settled for
// updateDebounce, in one "[Grimm update]" message.
func (s *Service) flushUpdates(ctx context.Context, tx pgx.Tx, sess Session) (bool, error) {
	var pending int
	var newest *time.Time
	if err := tx.QueryRow(ctx, `SELECT count(*), max(created_at) FROM grimm_wakeups WHERE session_id = $1 AND delivered_at IS NULL`, sess.ID).Scan(&pending, &newest); err != nil {
		return false, err
	}
	if pending == 0 || newest == nil || s.now().Sub(*newest) < updateDebounce {
		return false, nil
	}
	m, ok, err := currentManager(ctx, tx, sess.ID)
	if err != nil || !ok || m.Status != "live" || m.AgentID == "" {
		return false, err
	}
	var agentStatus string
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT status FROM agents WHERE host = $1 AND id = $2), ''),
		EXISTS (SELECT 1 FROM commands WHERE host = $1 AND kind = 'send_message' AND args->>'agentId' = $2 AND status IN ('queued', 'sent', 'running'))
		OR EXISTS (SELECT 1 FROM grimm_messages WHERE session_id = $3 AND kind = 'human' AND delivered_at IS NULL)`, sess.Host, m.AgentID, sess.ID).Scan(&agentStatus, &busy); err != nil {
		return false, err
	}
	if agentStatus != "idle" || busy {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT u.id, u.summary, u.detail, coalesce(w.status = 'archived', false)
		FROM grimm_wakeups u LEFT JOIN grimm_workers w ON w.session_id = u.session_id AND w.name = u.worker
		WHERE u.session_id = $1 AND u.delivered_at IS NULL ORDER BY u.id`, sess.ID)
	if err != nil {
		return false, err
	}
	type wk struct {
		id              int64
		summary, detail string
		archived        bool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (wk, error) {
		var x wk
		return x, r.Scan(&x.id, &x.summary, &x.detail, &x.archived)
	})
	if err != nil {
		return false, err
	}
	var ids, dropped []int64
	var summaries, details []string
	for _, u := range list {
		if u.archived { // the manager archived it since: it already wrapped it up
			dropped = append(dropped, u.id)
			continue
		}
		ids = append(ids, u.id)
		summaries = append(summaries, u.summary)
		details = append(details, u.detail)
	}
	if len(dropped) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE grimm_wakeups SET dropped = true, delivered_at = $2 WHERE id = ANY($1)`, dropped, s.now()); err != nil {
			return false, err
		}
	}
	if len(ids) == 0 {
		return true, nil
	}
	text := updateText(details)
	c, err := enqueue(ctx, tx, sess.Host, hostclient.SendMessageKind, hostclient.AgentArgs{AgentID: m.AgentID, Text: text, Behavior: hostclient.Steer}, commandTTL, s.now())
	if err != nil {
		return false, err
	}
	row, err := threadRow(ctx, tx, sess, "update", "intelligence", text, map[string]any{"summaries": summaries, "commandId": c.ID})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE grimm_wakeups SET delivered_at = $2, message_id = $3 WHERE id = ANY($1)`, ids, s.now(), row); err != nil {
		return false, err
	}
	return true, nil
}
