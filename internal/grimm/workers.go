package grimm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

// workerNames are given in order to hires that ask for no name.
var workerNames = []string{"athena", "hermes", "hephaestus", "apollo", "artemis", "prometheus", "minerva", "daedalus",
	"orion", "iris", "calliope", "atlas", "selene", "helios", "nike", "theseus", "ariadne", "perseus", "andromeda", "cassandra"}

// reserved names address something other than a worker.
var reserved = map[string]bool{ManagerName: true, "manager": true, "all": true, "human": true, "intelligence": true, "user": true}

// Worker is what the code needs of a grimm_workers row; DirectiveSeq marks the directives its first
// prompt already had.
type Worker struct {
	Name, Role, Provider, Model, Thinking, Login, LongWindow, Host, AgentID, Status, Task, LastResponse string
	DirectiveSeq                                                                                        int
}

// Hire asks for a worker. Provider may be provider/model; the role's policy can still refuse it.
type Hire struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // provider or provider/model; "" lets the balancer choose
	Task     string `json:"task"`
	Peers    []string
}

var nonName = regexp.MustCompile(`[^a-z0-9_-]+`)

// nextName is the hire's name: the one asked for (made unique), else the next free one.
func nextName(taken map[string]bool, want string) string {
	if want != "" {
		base := strings.Trim(nonName.ReplaceAllString(strings.ToLower(strings.TrimSpace(want)), "-"), "-_")
		if len(base) > 24 {
			base = base[:24]
		}
		if base != "" && !reserved[base] {
			if !taken[base] {
				return base
			}
			for i := 2; ; i++ {
				if n := fmt.Sprintf("%s-%d", base, i); !taken[n] {
					return n
				}
			}
		}
	}
	for _, n := range workerNames {
		if !taken[n] {
			return n
		}
	}
	for i := 1; ; i++ {
		if n := fmt.Sprintf("worker-%d", i); !taken[n] {
			return n
		}
	}
}

// hire hires one worker in sess (locked by the caller): under the cap, on the plan the balancer
// picks for its role, with a starting row and its create_agent command in the same transaction.
func (s *Service) hire(ctx context.Context, tx pgx.Tx, sess Session, h Hire, actor string) (Worker, Candidate, error) {
	task, err := cleanText("the task", h.Task)
	if err != nil {
		return Worker{}, Candidate{}, err
	}
	role := strings.ToLower(strings.TrimSpace(h.Role))
	if role == "" {
		role = "worker"
	}
	if !nameRe.MatchString(role) || role == "manager" {
		return Worker{}, Candidate{}, badRequest("a role is a short lowercase word, not manager")
	}
	if sess.WorkspaceID == "" {
		return Worker{}, Candidate{}, conflict("the session's worktree is not ready yet")
	}
	var live int
	taken := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT name, status FROM grimm_workers WHERE session_id = $1`, sess.ID)
	if err != nil {
		return Worker{}, Candidate{}, err
	}
	for rows.Next() {
		var n, st string
		if err := rows.Scan(&n, &st); err != nil {
			rows.Close()
			return Worker{}, Candidate{}, err
		}
		taken[n] = true
		if st == "starting" || st == "running" || st == "idle" {
			live++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Worker{}, Candidate{}, err
	}
	if live >= sess.MaxWorkers {
		return Worker{}, Candidate{}, conflict("live-worker cap reached (%d/%d): archive a finished worker before hiring another, or raise the cap", live, sess.MaxWorkers)
	}
	c, err := pick(ctx, tx, sess, role, h.Provider)
	if err != nil {
		return Worker{}, Candidate{}, err
	}
	name := nextName(taken, h.Name)
	directives, err := activeDirectives(ctx, tx, sess.ID)
	if err != nil {
		return Worker{}, Candidate{}, err
	}
	texts := make([]string, len(directives))
	maxSeq := 0
	for i, d := range directives {
		texts[i] = d.Text
		maxSeq = max(maxSeq, d.Seq)
	}
	if all, err := maxDirective(ctx, tx, sess.ID); err != nil {
		return Worker{}, Candidate{}, err
	} else {
		maxSeq = max(maxSeq, all)
	}
	peers := h.Peers
	if peers == nil {
		for n := range taken {
			peers = append(peers, n)
		}
	}
	cmd, err := enqueue(ctx, tx, sess.Host, hostclient.CreateAgentKind, hostclient.CreateAgentArgs{
		WorkspaceID: sess.WorkspaceID, Provider: c.Provider, Model: c.Model, Thinking: c.Thinking, Mode: c.Mode(),
		Title: fmt.Sprintf("Grimm/%s (%s)", name, role), Prompt: workerPrompt(sess, name, role, task, peers, texts),
		Labels: map[string]string{SessionLabel: sess.ID, RoleLabel: role, NameLabel: name, LiveLabel: "1"},
	}, commandTTL, s.now())
	if err != nil {
		return Worker{}, Candidate{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO grimm_workers (session_id, account_id, name, role, provider, model, thinking, mode, login,
		long_window, score, remaining_pct, host, command_id, task, directive_seq, turn_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17)`,
		sess.ID, sess.Account, name, role, c.Provider, c.Model, c.Thinking, c.Mode(), c.Login, c.LongWindow, c.Score, c.RemainingPct,
		sess.Host, cmd.ID, task, maxSeq, s.now()); err != nil {
		return Worker{}, Candidate{}, err
	}
	line := fmt.Sprintf("Hired %s (%s) on %s [%s %s, rem=%s, score=%s]", name, role, c.Spec(), c.Login, c.LongWindow, pct(c.RemainingPct), signed(c.Score))
	if _, err := busRow(ctx, tx, sess, "manager", name, "system", line); err != nil {
		return Worker{}, Candidate{}, err
	}
	if err := event(ctx, tx, sess, actor, "hire", "info", line, map[string]any{"worker": name, "role": role, "candidate": c, "task": task}); err != nil {
		return Worker{}, Candidate{}, err
	}
	w := Worker{Name: name, Role: role, Provider: c.Provider, Model: c.Model, Thinking: c.Thinking, Login: c.Login, LongWindow: c.LongWindow,
		Host: sess.Host, Status: "starting", Task: task, DirectiveSeq: maxSeq}
	return w, c, notify(ctx, tx, sess.ID)
}

// HiredLine starts with "hired <name> (<role>) on <provider>/<model>": the page's labeller parses it.
func HiredLine(w Worker) string {
	return fmt.Sprintf("hired %s (%s) on %s/%s [%s %s]", w.Name, w.Role, w.Provider, w.Model, w.Login, w.LongWindow)
}

func worker(ctx context.Context, tx pgx.Tx, session, name string) (Worker, error) {
	var w Worker
	err := tx.QueryRow(ctx, `SELECT name, role, provider, model, thinking, login, long_window, host, agent_id, status, task, last_response, directive_seq
		FROM grimm_workers WHERE session_id = $1 AND name = $2`, session, strings.ToLower(strings.TrimSpace(name))).
		Scan(&w.Name, &w.Role, &w.Provider, &w.Model, &w.Thinking, &w.Login, &w.LongWindow, &w.Host, &w.AgentID, &w.Status, &w.Task, &w.LastResponse, &w.DirectiveSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, refuse(404, "no worker named %s in this session", name)
	}
	return w, err
}

// tell sends text into a worker's session (a message from the manager, the human or a peer),
// with any directive it has not had yet, and marks it working on a new turn.
func (s *Service) tell(ctx context.Context, tx pgx.Tx, sess Session, w Worker, text string) error {
	switch w.Status {
	case "archived":
		return conflict("%s is archived", w.Name)
	case "failed":
		return conflict("%s failed to start", w.Name)
	}
	if w.AgentID == "" {
		return conflict("%s is still starting", w.Name)
	}
	pending, err := pendingDirectives(ctx, tx, sess.ID, w)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		var b strings.Builder
		for _, d := range pending {
			fmt.Fprintf(&b, "[Grimm directive #%d, now standing for every agent] %s\n", d.Seq, d.Text)
		}
		text = b.String() + "\n" + text
	}
	if _, err := enqueue(ctx, tx, sess.Host, hostclient.SendMessageKind, hostclient.AgentArgs{AgentID: w.AgentID, Text: text, Behavior: hostclient.Steer}, commandTTL, s.now()); err != nil {
		return err
	}
	for _, d := range pending {
		if _, err := tx.Exec(ctx, `INSERT INTO grimm_directive_deliveries (session_id, account_id, worker, seq) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, sess.ID, sess.Account, w.Name, d.Seq); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE grimm_workers SET status = 'running', seen_running = false, result_posted = false, turn_at = $3, finished_at = NULL
		WHERE session_id = $1 AND name = $2`, sess.ID, w.Name, s.now())
	return err
}

// say is a message to a worker from the manager or the human, also posted on the bus.
func (s *Service) say(ctx context.Context, tx pgx.Tx, sess Session, from, name, text string) (string, error) {
	text, err := cleanText("the message", text)
	if err != nil {
		return "", err
	}
	w, err := worker(ctx, tx, sess.ID, name)
	if err != nil {
		return "", err
	}
	header := "[Message from the Grimm manager]"
	if from == "human" {
		header = "[Message from the human, through the Grimm page]"
	}
	if err := s.tell(ctx, tx, sess, w, header+"\n\n"+text); err != nil {
		return "", err
	}
	if _, err := busRow(ctx, tx, sess, from, w.Name, "chat", text); err != nil {
		return "", err
	}
	if err := event(ctx, tx, sess, from, "say", "info", "Messaged "+w.Name, map[string]string{"worker": w.Name}); err != nil {
		return "", err
	}
	return "sent to " + w.Name, notify(ctx, tx, sess.ID)
}

func (s *Service) stop(ctx context.Context, tx pgx.Tx, sess Session, actor, name string) (string, error) {
	w, err := worker(ctx, tx, sess.ID, name)
	if err != nil {
		return "", err
	}
	if w.AgentID == "" || w.Status == "archived" {
		return "", conflict("%s is not running", w.Name)
	}
	if _, err := enqueue(ctx, tx, sess.Host, hostclient.CancelAgentKind, hostclient.AgentArgs{AgentID: w.AgentID}, commandTTL, s.now()); err != nil {
		return "", err
	}
	if err := event(ctx, tx, sess, actor, "stop", "info", "Stopped "+w.Name+"'s turn", map[string]string{"worker": w.Name}); err != nil {
		return "", err
	}
	return "stopped " + w.Name, notify(ctx, tx, sess.ID)
}

// archiveWorkers archives one worker, or with name "" every one not yet archived. Their files stay.
func (s *Service) archiveWorkers(ctx context.Context, tx pgx.Tx, sess Session, name, actor string) ([]string, error) {
	q := `SELECT name, agent_id FROM grimm_workers WHERE session_id = $1 AND status <> 'archived' AND ($2 = '' OR name = $2) ORDER BY created_at`
	rows, err := tx.Query(ctx, q, sess.ID, name)
	if err != nil {
		return nil, err
	}
	type wa struct{ name, agent string }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (wa, error) {
		var x wa
		return x, r.Scan(&x.name, &x.agent)
	})
	if err != nil {
		return nil, err
	}
	if name != "" && len(list) == 0 {
		if _, err := worker(ctx, tx, sess.ID, name); err != nil {
			return nil, err
		}
		return nil, conflict("%s is already archived", name)
	}
	var names []string
	for _, x := range list {
		if x.agent != "" {
			if _, err := enqueue(ctx, tx, sess.Host, hostclient.ArchiveAgentKind, hostclient.AgentArgs{AgentID: x.agent}, commandTTL, s.now()); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_workers SET status = 'archived', archived_at = $3 WHERE session_id = $1 AND name = $2`, sess.ID, x.name, s.now()); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_wakeups SET dropped = true, delivered_at = $3 WHERE session_id = $1 AND worker = $2 AND delivered_at IS NULL`,
			sess.ID, x.name, s.now()); err != nil {
			return nil, err
		}
		names = append(names, x.name)
	}
	if len(names) > 0 {
		if err := event(ctx, tx, sess, actor, "archive", "info", "Archived "+strings.Join(names, ", "), map[string]any{"workers": names}); err != nil {
			return nil, err
		}
	}
	return names, notify(ctx, tx, sess.ID)
}

// Directive is a standing rule every agent of the session inherits.
type Directive struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
	By   string `json:"addedBy"`
}

func activeDirectives(ctx context.Context, tx pgx.Tx, session string) ([]Directive, error) {
	rows, err := tx.Query(ctx, `SELECT seq, text, added_by FROM grimm_directives WHERE session_id = $1 AND removed_at IS NULL ORDER BY seq`, session)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Directive, error) {
		var d Directive
		return d, r.Scan(&d.Seq, &d.Text, &d.By)
	})
}

func maxDirective(ctx context.Context, tx pgx.Tx, session string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM grimm_directives WHERE session_id = $1`, session).Scan(&n)
	return n, err
}

// pendingDirectives are the standing directives added after w was hired that it has not had.
func pendingDirectives(ctx context.Context, tx pgx.Tx, session string, w Worker) ([]Directive, error) {
	rows, err := tx.Query(ctx, `SELECT d.seq, d.text, d.added_by FROM grimm_directives d WHERE d.session_id = $1 AND d.removed_at IS NULL
		AND d.seq > $3 AND NOT EXISTS (SELECT 1 FROM grimm_directive_deliveries x WHERE x.session_id = d.session_id AND x.worker = $2 AND x.seq = d.seq)
		ORDER BY d.seq`, session, w.Name, w.DirectiveSeq)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Directive, error) {
		var d Directive
		return d, r.Scan(&d.Seq, &d.Text, &d.By)
	})
}

// addDirective records a standing rule and pushes it at once to every worker on a turn; an idle
// one gets it with its next message. It returns who it was pushed to.
func (s *Service) addDirective(ctx context.Context, tx pgx.Tx, sess Session, by, text string) (int, []string, error) {
	text, err := cleanText("the directive", text)
	if err != nil {
		return 0, nil, err
	}
	if len(text) > 2000 {
		return 0, nil, badRequest("a directive is at most 2000 bytes")
	}
	seq, err := maxDirective(ctx, tx, sess.ID)
	if err != nil {
		return 0, nil, err
	}
	seq++
	if _, err := tx.Exec(ctx, `INSERT INTO grimm_directives (session_id, account_id, seq, text, added_by) VALUES ($1, $2, $3, $4, $5)`,
		sess.ID, sess.Account, seq, text, by); err != nil {
		return 0, nil, err
	}
	if _, err := busRow(ctx, tx, sess, by, "all", "directive", text); err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT name FROM grimm_workers WHERE session_id = $1 AND status = 'running' AND agent_id <> '' ORDER BY name`, sess.ID)
	if err != nil {
		return 0, nil, err
	}
	busy, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, nil, err
	}
	var pushed []string
	for _, n := range busy {
		w, err := worker(ctx, tx, sess.ID, n)
		if err != nil {
			return 0, nil, err
		}
		pending, err := pendingDirectives(ctx, tx, sess.ID, w)
		if err != nil {
			return 0, nil, err
		}
		var b strings.Builder
		for _, d := range pending {
			fmt.Fprintf(&b, "[Grimm directive #%d, now standing for every agent] %s\n", d.Seq, d.Text)
		}
		if b.Len() == 0 {
			continue
		}
		if _, err := enqueue(ctx, tx, sess.Host, hostclient.SendMessageKind, hostclient.AgentArgs{AgentID: w.AgentID, Text: b.String(), Behavior: hostclient.Steer}, commandTTL, s.now()); err != nil {
			return 0, nil, err
		}
		for _, d := range pending {
			if _, err := tx.Exec(ctx, `INSERT INTO grimm_directive_deliveries (session_id, account_id, worker, seq) VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`, sess.ID, sess.Account, w.Name, d.Seq); err != nil {
				return 0, nil, err
			}
		}
		pushed = append(pushed, w.Name)
	}
	if err := event(ctx, tx, sess, by, "directive_add", "info", fmt.Sprintf("Added directive #%d: %s", seq, cut(text, 200)), map[string]any{"seq": seq, "pushedTo": pushed}); err != nil {
		return 0, nil, err
	}
	return seq, pushed, notify(ctx, tx, sess.ID)
}

func (s *Service) removeDirective(ctx context.Context, tx pgx.Tx, sess Session, by string, seq int) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `UPDATE grimm_directives SET removed_at = $3 WHERE session_id = $1 AND seq = $2 AND removed_at IS NULL RETURNING text`,
		sess.ID, seq, s.now()).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse(404, "no standing directive #%d", seq)
	}
	if err != nil {
		return "", err
	}
	if err := event(ctx, tx, sess, by, "directive_remove", "info", fmt.Sprintf("Removed directive #%d: %s", seq, cut(text, 200)), map[string]int{"seq": seq}); err != nil {
		return "", err
	}
	return text, notify(ctx, tx, sess.ID)
}

// post puts a message on the bus from sender. One to a worker goes straight into its session; one
// to grimm wakes the manager.
func (s *Service) post(ctx context.Context, tx pgx.Tx, sess Session, sender, to, kind, body string) (string, error) {
	body, err := cleanText("the message", body)
	if err != nil {
		return "", err
	}
	to = strings.ToLower(strings.TrimSpace(to))
	if to == "" || to == "manager" {
		to = ManagerName
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "chat"
	}
	switch kind {
	case "chat", "handoff", "result", "question":
	default:
		return "", badRequest("--kind is chat, handoff, question or result")
	}
	seq, err := busRow(ctx, tx, sess, sender, to, kind, body)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("posted #%d to %s", seq, to)
	switch to {
	case "all":
	case ManagerName:
		if kind == "result" {
			if _, err := tx.Exec(ctx, `UPDATE grimm_workers SET result_posted = true WHERE session_id = $1 AND name = $2`, sess.ID, sender); err != nil {
				return "", err
			}
		}
		if err := s.wake(ctx, tx, sess, sender, sender+" → grimm ("+kind+")",
			fmt.Sprintf("Bus #%d from **%s** to you (%s): %s", seq, sender, kind, cut(body, 2000))); err != nil {
			return "", err
		}
	default:
		w, err := worker(ctx, tx, sess.ID, to)
		if err != nil {
			return "", err
		}
		if w.Name == sender {
			return "", badRequest("that is you")
		}
		text := fmt.Sprintf("[Grimm bus #%d: %s from %s]\n\n%s\n\n(Reply with `%s post --to %s --kind handoff - <<'EOF'`.)", seq, kind, sender, body, busCmd, sender)
		if err := s.tell(ctx, tx, sess, w, text); err != nil {
			return "", err
		}
		out += " (delivered into its session)"
	}
	return out, notify(ctx, tx, sess.ID)
}

// wake queues news for the manager; flushUpdates batches it into one message once the manager is idle.
func (s *Service) wake(ctx context.Context, tx pgx.Tx, sess Session, worker, summary, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO grimm_wakeups (session_id, account_id, worker, summary, detail, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		sess.ID, sess.Account, worker, summary, detail, s.now())
	return err
}
