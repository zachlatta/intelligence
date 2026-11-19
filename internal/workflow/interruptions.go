package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Login recovery bounds. An agent resumed this many times within the window and failing again is
// left for someone to look at; an interruption with no working login for a day is given up on.
const (
	recoveryLimit  = 3
	recoveryWindow = 6 * time.Hour
	recoveryExpiry = 24 * time.Hour
)

type interruption struct {
	Host, Agent, Provider, Model, Title, Message, Reason string
	Seq                                                  int
	At                                                   time.Time
	Run                                                  *string
	Login                                                string // the broken login, '' unknown
}

// RecoverInterruptions acts on each agent turn a broken login stopped (agent_interruptions,
// recorded as transcripts arrive): once its host has a working login again, it asks the agent to
// carry on (the same agent: same model, its context intact), or, for a sub-agent, tells its parent
// to resume or replace it. Each interruption is acted on once, under its row lock, so several
// servers can run this; every decision is recorded on the row and on the run.
func (e *Engine) RecoverInterruptions(ctx context.Context) error {
	rows, err := e.p.Query(ctx, `SELECT host, agent_id, seq FROM agent_interruptions WHERE status = 'waiting'
		ORDER BY at, host, agent_id, seq LIMIT 200`)
	if err != nil {
		return err
	}
	type key struct {
		host, agent string
		seq         int
	}
	var keys []key
	var k key
	if _, err := pgx.ForEachRow(rows, []any{&k.host, &k.agent, &k.seq}, func() error { keys = append(keys, k); return nil }); err != nil {
		return err
	}
	var failures []error
	for _, k := range keys {
		err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
			var x interruption
			err := tx.QueryRow(ctx, `SELECT x.host, x.agent_id, x.seq, x.at, x.provider, x.model, x.title, x.message, x.reason,
				x.run_id::text, coalesce(i.login, '')
				FROM agent_interruptions x LEFT JOIN login_incidents i ON i.id = x.incident_id
				WHERE x.host = $1 AND x.agent_id = $2 AND x.seq = $3 AND x.status = 'waiting' FOR UPDATE OF x SKIP LOCKED`,
				k.host, k.agent, k.seq).Scan(&x.Host, &x.Agent, &x.Seq, &x.At, &x.Provider, &x.Model, &x.Title, &x.Message, &x.Reason, &x.Run, &x.Login)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return e.recoverInterruption(ctx, tx, x)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("agent %s on %s: %w", k.agent, k.host, err))
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) recoverInterruption(ctx context.Context, tx pgx.Tx, x interruption) error {
	now := e.Now()
	if now.Sub(x.At) > recoveryExpiry {
		return e.settle(ctx, tx, x, "expired", "", nil, "No working login for 24 hours, so Intelligence stopped waiting. Sign the login in again, then resume the agent by hand.")
	}
	var status, parent, model, title string
	err := tx.QueryRow(ctx, "SELECT status, parent_id, model, title FROM agents WHERE host = $1 AND id = $2", x.Host, x.Agent).
		Scan(&status, &parent, &model, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		return e.settle(ctx, tx, x, "gone", "", nil, "The agent is no longer on "+x.Host+", so there is nothing to resume.")
	}
	if err != nil {
		return err
	}
	if model != "" {
		x.Model = model
	}
	if title != "" {
		x.Title = title
	}
	// a newer turn in its transcript (not itself a login failure): something else got it going
	var moved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcript_entries t WHERE t.host = $1 AND t.agent_id = $2 AND t.seq_start > $3
		AND t.kind IN ('assistant_message', 'tool_call', 'reasoning')
		AND NOT EXISTS (SELECT 1 FROM agent_interruptions o WHERE o.host = t.host AND o.agent_id = t.agent_id AND o.seq = t.seq_start))`,
		x.Host, x.Agent, x.Seq).Scan(&moved); err != nil {
		return err
	}
	if moved {
		return e.settle(ctx, tx, x, "superseded", "", nil, "It carried on before Intelligence resumed it.")
	}
	// it failed again later (a nudge, or a message, on the same dead login): one resume, for the
	// newest failure, is enough
	var later bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_interruptions WHERE host = $1 AND agent_id = $2 AND seq > $3
		AND status = 'waiting')`, x.Host, x.Agent, x.Seq).Scan(&later); err != nil {
		return err
	}
	if later {
		return e.settle(ctx, tx, x, "superseded", "", nil, "It failed again later on the same login; Intelligence resumes it once, for that failure.")
	}
	if status == "running" || status == "initializing" {
		return nil // busy: see what its turn does
	}
	healthy, err := loginHealthy(ctx, tx, x, now)
	if err != nil || !healthy {
		return err
	}
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_interruptions WHERE host = $1 AND agent_id = $2
		AND status IN ('resumed', 'parent_notified') AND acted_at > $3`, x.Host, x.Agent, now.Add(-recoveryWindow)).Scan(&recent); err != nil {
		return err
	}
	if recent >= recoveryLimit {
		return e.settle(ctx, tx, x, "gave_up", "", nil, fmt.Sprintf("It failed again after %d recoveries in %s, so Intelligence stopped. Check the host's login, then resume the agent by hand.", recent, recoveryWindow))
	}
	who := describe(x)
	target, text, outcome := x.Agent, resumeText(x), "Resumed "+who+": asked it to carry on where it stopped, now that a working login is active."
	if parent != "" {
		var parentLive, parentWaiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE host = $1 AND id = $2),
			EXISTS (SELECT 1 FROM agent_interruptions WHERE host = $1 AND agent_id = $2 AND status = 'waiting')`, x.Host, parent).
			Scan(&parentLive, &parentWaiting); err != nil {
			return err
		}
		if parentWaiting {
			return nil // the parent first; it hears about this one after
		}
		if parentLive {
			target, text = parent, parentText(x)
			outcome = "Asked its parent agent to resume or replace " + who + " with the same model."
		}
	}
	if target == x.Agent {
		// a message sent to it since it failed (Zach's, say) is on its way and gets it going: wait for
		// what that turn does rather than send a second one
		var onItsWay bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands WHERE host = $1 AND kind = $2 AND args->>'agentId' = $3
			AND status IN ('queued', 'sent', 'running') AND expires_at > $4 AND created_at >= $5)`,
			x.Host, hostclient.SendMessageKind, x.Agent, now, x.At).Scan(&onItsWay); err != nil {
			return err
		}
		if onItsWay {
			return nil
		}
	}
	args, _ := json.Marshal(hostclient.AgentArgs{AgentID: target, Text: text, Behavior: hostclient.Queue})
	c, err := fleet.EnqueueTx(ctx, tx, x.Host, hostclient.SendMessageKind, args, 10*time.Minute, now)
	if err != nil {
		return err
	}
	state := "resumed"
	if target != x.Agent {
		state = "parent_notified"
	}
	return e.settle(ctx, tx, x, state, target, &c.ID, outcome)
}

// loginHealthy: the host is connected, and the login its tool uses now is not broken: cswap failed
// over to another login, or the broken one works again (or was marked fixed). A host whose login is
// unknown has its broken-login incident on ”, which only a working reading or a fix closes.
func loginHealthy(ctx context.Context, tx pgx.Tx, x interruption, now time.Time) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `WITH cur AS (
		SELECT coalesce((SELECT account FROM usage_active WHERE host = $1 AND provider = $2 ORDER BY since DESC LIMIT 1), '') AS login)
		SELECT EXISTS (SELECT 1 FROM hosts h WHERE h.name = $1 AND h.connected AND h.last_seen > $3::timestamptz - interval '1 minute')
		AND NOT EXISTS (SELECT 1 FROM login_incidents i, cur WHERE i.host = $1 AND i.provider = $2 AND i.resolved_at IS NULL
			AND (i.login = cur.login OR i.login = ''))
		FROM cur`, x.Host, x.Provider, now).Scan(&ok)
	return ok, err
}

// settle records what was done about an interruption, and tells its run.
func (e *Engine) settle(ctx context.Context, tx pgx.Tx, x interruption, status, target string, command *string, outcome string) error {
	now := e.Now()
	if _, err := tx.Exec(ctx, `UPDATE agent_interruptions SET status = $4, target_agent_id = $5, command_id = $6::uuid, outcome = $7, acted_at = $8
		WHERE host = $1 AND agent_id = $2 AND seq = $3`, x.Host, x.Agent, x.Seq, status, target, command, outcome, now); err != nil {
		return err
	}
	if x.Run == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text)
		SELECT id, account_id, $2, 'login_recovery', step, $3 FROM workflow_runs WHERE id = $1`, *x.Run, now, outcome)
	return err
}

func describe(x interruption) string {
	who := x.Title
	if who == "" {
		who = "agent " + x.Agent
	}
	if x.Model != "" {
		who += " (" + x.Provider + "/" + x.Model + ")"
	}
	return who
}

func loginPhrase(x interruption) string {
	if x.Login != "" {
		return "the host's " + x.Provider + " login " + x.Login
	}
	return "the host's " + x.Provider + " login"
}

const noRepeat = "Before repeating any external action (a push, a deploy, an email, a payment, a message), check whether it already happened."

func resumeText(x interruption) string {
	return fmt.Sprintf("Intelligence: your last turn did not run. At %s %s stopped working, and the provider answered %q. "+
		"A working login is active now, so carry on exactly where you left off: your context is intact and your last instructions still stand. "+
		"If you started sub-agents, check whether they were interrupted too. %s",
		x.At.UTC().Format(time.RFC3339), loginPhrase(x), x.Message, noRepeat)
}

func parentText(x interruption) string {
	provider := x.Provider
	if x.Model != "" {
		provider += "/" + x.Model
	}
	return fmt.Sprintf("Intelligence: your sub-agent %s (%s) stopped at %s: its turn did not run because %s stopped working (%q). "+
		"A working login is active now. Resume it, so it keeps its context and model: "+
		"`paseo send %s \"Your last turn did not run because the login stopped working; it works again. Carry on where you left off. %s\"`. "+
		"If it is gone or cannot continue, start a replacement with the same model (`--provider %s`, in the same workspace and mode) and give it the context it needs. "+
		"Check its earlier outcome first. %s",
		describe(x), x.Agent, x.At.UTC().Format(time.RFC3339), loginPhrase(x), x.Message, x.Agent, noRepeat, provider, noRepeat)
}

// RunInterruptionRecovery stops with the server. More than one instance may safely run it.
func (e *Engine) RunInterruptionRecovery(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := e.RecoverInterruptions(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("login recovery: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
