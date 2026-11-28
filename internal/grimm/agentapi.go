package grimm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// Call is one `intelligence grimm ctl|bus` command from an agent on a host. The host's key says
// which host (and account) is calling, and the agent's PASEO_AGENT_ID which of that host's Grimm
// agents: it is the caller, so a worker that follows its prompt cannot post as another or act as
// the manager. It is not a wall between one host's agents: each can read the host's key and name
// another agent, as with `intelligence workflow`. The host's key is the boundary.
type Call struct {
	Verb     string   `json:"verb"`
	Args     []string `json:"args"`
	Text     string   `json:"text"`     // a message, task or directive (from stdin or --file)
	Role     string   `json:"role"`     // hire
	Name     string   `json:"name"`     // hire
	Provider string   `json:"provider"` // hire
	To       string   `json:"to"`       // bus post
	Kind     string   `json:"kind"`     // bus post
	Lines    int      `json:"lines"`    // peek, bus read
	Timeout  int      `json:"timeout"`  // wait, in seconds
	Workflow string   `json:"workflow"` // workflow request
	Summary  string   `json:"summary"`  // workflow request
	Host     string   `json:"host"`     // workflow request
	Replaces string   `json:"replaces"` // workflow request --replaces
	Repo     string   `json:"repo"`     // workflow request --repo
	Reason   string   `json:"reason"`   // workflow withdraw --reason / cancel --why
	Since    int64    `json:"since"`    // workflow events --since
	Agent    string   `json:"agent"`    // workflow peek --agent
}

// Caller is who an agent call is from.
type Caller struct {
	Session string
	Role    string // manager or worker
	Name    string
}

// MaxWait bounds `ctl wait`.
const MaxWait = 10 * time.Minute

// whoIs finds agent on host in account's active sessions.
func whoIs(ctx context.Context, tx pgx.Tx, account int64, host, agent string) (Caller, error) {
	var c Caller
	if agent == "" {
		return c, refuse(403, "run this from inside a Grimm agent (PASEO_AGENT_ID is not set)")
	}
	err := tx.QueryRow(ctx, `SELECT g.session_id::text, g.role, g.name FROM grimm_agents g JOIN grimm_sessions s ON s.id = g.session_id
		WHERE g.account_id = $1 AND g.host = $2 AND g.agent_id = $3 AND s.status = 'active'
		ORDER BY g.role = 'manager' DESC, g.epoch DESC NULLS LAST LIMIT 1`, account, host, agent).Scan(&c.Session, &c.Role, &c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, refuse(403, "this agent is not part of an active Grimm session")
	}
	return c, err
}

// managerVerbs are what only the manager may do.
var managerVerbs = map[string]bool{"hire": true, "say": true, "stop": true, "archive": true, "wait": true, "peek": true, "result": true}

// AgentCall runs an agent's command and returns what it prints.
func (s *Service) AgentCall(ctx context.Context, changes *fleet.Changes, account int64, host, agent string, c Call) (string, error) {
	if c.Verb == "wait" {
		return s.wait(ctx, changes, account, host, agent, c)
	}
	if c.Verb == "workflow" && len(c.Args) > 0 {
		switch c.Args[0] {
		case "request":
			return s.requestWorkflow(ctx, account, host, agent, c)
		case "withdraw":
			return s.withdrawWorkflow(ctx, account, host, agent, c)
		case "input":
			return s.inputWorkflow(ctx, account, host, agent, c)
		case "reply":
			return s.replyWorkflow(ctx, account, host, agent, c)
		case "cancel":
			return s.cancelWorkflow(ctx, account, host, agent, c)
		}
	}
	var out string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		who, err := whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		sess, err := activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		if c.Verb == "directive" && len(c.Args) > 0 && c.Args[0] != "list" && who.Role != "manager" {
			return refuse(403, "only the Grimm manager changes directives; ask it on the bus (--to grimm)")
		}
		if managerVerbs[c.Verb] && who.Role != "manager" {
			return refuse(403, "only the Grimm manager can %s; post to grimm on the bus instead", c.Verb)
		}
		sender := who.Name
		arg := func(i int) string {
			if i < len(c.Args) {
				return c.Args[i]
			}
			return ""
		}
		switch c.Verb {
		case "quota":
			cs, err := quota(ctx, tx, sess.ID)
			out = QuotaTable(Judge("worker", cs, nil))
			return err
		case "roster":
			out, err = roster(ctx, tx, sess.ID)
			return err
		case "hire":
			w, _, err := s.hire(ctx, tx, sess, Hire{Role: c.Role, Name: c.Name, Provider: c.Provider, Task: c.Text}, sender)
			out = HiredLine(w)
			return err
		case "say":
			if arg(0) == "" {
				return badRequest("say needs a worker's name")
			}
			out, err = s.say(ctx, tx, sess, "manager", arg(0), c.Text)
			return err
		case "peek":
			w, err := worker(ctx, tx, sess.ID, arg(0))
			if err != nil {
				return err
			}
			if c.Lines == 0 {
				c.Lines = 40
			}
			out, err = peek(ctx, tx, w, c.Lines)
			return err
		case "result":
			w, err := worker(ctx, tx, sess.ID, arg(0))
			if err != nil {
				return err
			}
			out = w.LastResponse
			if out == "" {
				out = "(" + w.Name + " has no reply yet; it is " + w.Status + ")"
			}
			return nil
		case "stop":
			out, err = s.stop(ctx, tx, sess, sender, arg(0))
			return err
		case "archive":
			target := arg(0)
			if target == "" {
				return badRequest("archive needs a worker's name, or all")
			}
			if target == "all" {
				target = ""
			}
			names, err := s.archiveWorkers(ctx, tx, sess, target, sender)
			out = "archived: " + strings.Join(names, ", ")
			if len(names) == 0 {
				out = "archived: (none)"
			}
			return err
		case "directive":
			switch arg(0) {
			case "", "list":
				out, err = directivesText(ctx, tx, sess.ID)
			case "add":
				text := c.Text
				if text == "" {
					text = strings.Join(c.Args[1:], " ")
				}
				seq, pushed, aerr := s.addDirective(ctx, tx, sess, sender, text)
				out, err = directiveAdded(seq, strings.TrimSpace(text), pushed), aerr
			case "remove":
				var n int
				if _, serr := fmt.Sscan(strings.TrimPrefix(arg(1), "#"), &n); serr != nil {
					return badRequest("directive remove <n>")
				}
				removed, rerr := s.removeDirective(ctx, tx, sess, sender, n)
				out, err = "removed: "+removed, rerr
			default:
				return badRequest("directive list | add <rule> | remove <n>")
			}
			return err
		case "workflow":
			switch arg(0) {
			case "catalog":
				out, err = workflowCatalogText(ctx, tx, sess.Account)
			case "", "list":
				out, err = workflowListText(ctx, tx, sess.ID)
			case "status":
				out, err = workflowStatusText(ctx, tx, sess.ID, arg(1))
			case "events":
				out, err = workflowEventsText(ctx, tx, sess.ID, arg(1), c.Since, c.Lines)
			case "peek":
				out, err = workflowPeekText(ctx, tx, sess.ID, arg(1), c.Agent, c.Lines)
			default:
				return badRequest("workflow catalog | request | withdraw | list | status | events | peek | input | reply | cancel")
			}
			return err
		case "bus-read":
			n := c.Lines
			if n == 0 {
				n = 30
			}
			forAgent := ""
			if who.Role != "manager" && arg(0) == "mine" {
				forAgent = who.Name
			}
			out, err = busText(ctx, tx, sess.ID, forAgent, n)
			return err
		case "bus-post":
			from := who.Name
			if who.Role == "manager" {
				from = ManagerName
			}
			out, err = s.post(ctx, tx, sess, from, c.To, c.Kind, c.Text)
			return err
		}
		return badRequest("unknown command %q", c.Verb)
	})
	return out, err
}

// wait blocks until the named workers (or every working one) have finished their turns, or the
// timeout, waking on each change to the session.
func (s *Service) wait(ctx context.Context, changes *fleet.Changes, account int64, host, agent string, c Call) (string, error) {
	timeout := time.Duration(c.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timeout = min(timeout, MaxWait)
	var who Caller
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		who, err = whoIs(ctx, tx, account, host, agent)
		if err == nil && who.Role != "manager" {
			err = refuse(403, "only the Grimm manager can wait")
		}
		return err
	}); err != nil {
		return "", err
	}
	var wakeCh <-chan struct{}
	if changes != nil {
		ch, stop := changes.Subscribe(fleet.GrimmTopic(who.Session))
		defer stop()
		wakeCh = ch
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		busy, summary, err := s.waitState(ctx, who.Session, c.Args)
		if err != nil {
			return "", err
		}
		if len(busy) == 0 {
			return summary, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "still working after " + timeout.String() + ": " + strings.Join(busy, ", ") + "\n" + summary, nil
		case <-wakeCh:
		case <-tick.C:
		}
	}
}

func (s *Service) waitState(ctx context.Context, session string, names []string) ([]string, string, error) {
	rows, err := s.DB.Query(ctx, `SELECT name, status FROM grimm_workers WHERE session_id = $1
		AND (cardinality($2::text[]) = 0 OR name = ANY($2)) AND status <> 'archived' ORDER BY created_at`, session, names)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var busy []string
	var b strings.Builder
	seen := map[string]bool{}
	for rows.Next() {
		var n, st string
		if err := rows.Scan(&n, &st); err != nil {
			return nil, "", err
		}
		seen[n] = true
		if st == "running" || st == "starting" {
			busy = append(busy, n)
		}
		fmt.Fprintf(&b, "%s: %s\n", n, st)
	}
	for _, n := range names {
		if !seen[strings.ToLower(n)] {
			fmt.Fprintf(&b, "%s: no such live worker\n", n)
		}
	}
	return busy, strings.TrimRight(b.String(), "\n"), rows.Err()
}
