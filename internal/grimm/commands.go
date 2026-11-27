package grimm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/attachment"
)

// Help is what /help answers.
const Help = `Talk to Grimm in plain words; it plans, hires and reports back. Power-user commands run without a manager turn:

@name <message>        message a worker directly
/quota                 plan headroom and where the next hire goes
/roster                who is hired
/spawn [name] [--role R] [--provider P] <task>   hire one worker yourself
/fanout <N> [--review] <task>   hire N workers on the same task (and a reviewer)
/remember <rule>       add a standing directive
/forget <n>            remove directive n
/directives            list the standing directives
/bus [N]               the last N bus messages
/stop [name]           interrupt Grimm's turn, or a worker's
/archive [name|all]    archive a worker, or every one
/cap <N>               set the live-worker cap
/new                   start a fresh manager conversation
/help                  this list

A message with attachments always goes to Grimm.`

var atWorker = regexp.MustCompile(`(?s)^@([a-z0-9_-]+)[\s,:]+(.+)$`)

// Post is a message typed on the page: to Grimm, to a worker (@name) or a command (/...).
// It returns the thread row it added.
func (s *Service) Post(ctx context.Context, account int64, id, text string) (int64, error) {
	text, err := cleanText("the message", text)
	if err != nil {
		return 0, err
	}
	var row int64
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		// a message with images or files always goes to the manager, whatever its first word
		if attachment.Reference.MatchString(text) || !(strings.HasPrefix(text, "/") || strings.HasPrefix(text, "@")) {
			row, err = s.SendToManager(ctx, tx, sess, text)
			return err
		}
		if m := atWorker.FindStringSubmatch(strings.ToLower(text[:min(len(text), 40)]) + text[min(len(text), 40):]); m != nil {
			if m[1] == ManagerName {
				row, err = s.SendToManager(ctx, tx, sess, strings.TrimSpace(text[len(m[1])+1:]))
				return err
			}
			body := strings.TrimSpace(text[len(m[1])+1:])
			body = strings.TrimLeft(body, ",: ")
			row, err = s.command(ctx, tx, sess, text, func() (string, error) { return s.say(ctx, tx, sess, "human", m[1], body) })
			return err
		}
		if strings.HasPrefix(text, "@") {
			return badRequest("write @name and a message")
		}
		row, err = s.slash(ctx, tx, sess, text)
		return err
	})
	return row, err
}

// command runs fn and records it as a command row in the thread: its output, or why it failed.
func (s *Service) command(ctx context.Context, tx pgx.Tx, sess Session, text string, fn func() (string, error)) (int64, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	out, ferr := fn()
	status := "done"
	if ferr != nil {
		_ = sp.Rollback(ctx)
		e, ok := ferr.(*Error)
		if !ok {
			return 0, ferr
		}
		out, status = e.Msg, "error"
	} else if err := sp.Commit(ctx); err != nil {
		return 0, err
	}
	id, err := threadRow(ctx, tx, sess, "command", "human", text, map[string]string{"output": out, "status": status})
	if err != nil {
		return 0, err
	}
	return id, notify(ctx, tx, sess.ID)
}

func (s *Service) slash(ctx context.Context, tx pgx.Tx, sess Session, text string) (int64, error) {
	fields := strings.Fields(text)
	cmd, rest := strings.ToLower(fields[0]), fields[1:]
	restText := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	run := func(fn func() (string, error)) (int64, error) { return s.command(ctx, tx, sess, text, fn) }
	switch cmd {
	case "/help", "/?":
		return run(func() (string, error) { return Help, nil })
	case "/quota":
		return run(func() (string, error) {
			cs, err := quota(ctx, tx, sess.ID)
			return QuotaTable(Judge("worker", cs, nil)), err
		})
	case "/roster":
		return run(func() (string, error) { return roster(ctx, tx, sess.ID) })
	case "/bus":
		n := 30
		if len(rest) > 0 {
			if v, err := strconv.Atoi(rest[0]); err == nil {
				n = v
			}
		}
		return run(func() (string, error) { return busText(ctx, tx, sess.ID, "", n) })
	case "/directives":
		return run(func() (string, error) { return directivesText(ctx, tx, sess.ID) })
	case "/remember":
		return run(func() (string, error) {
			seq, pushed, err := s.addDirective(ctx, tx, sess, "human", restText)
			return directiveAdded(seq, restText, pushed), err
		})
	case "/forget":
		return run(func() (string, error) {
			n, err := strconv.Atoi(strings.TrimPrefix(restText, "#"))
			if err != nil {
				return "", badRequest("usage: /forget <n>")
			}
			removed, err := s.removeDirective(ctx, tx, sess, "human", n)
			return "removed: " + removed, err
		})
	case "/stop":
		if len(rest) > 0 {
			return run(func() (string, error) { return s.stop(ctx, tx, sess, "human", rest[0]) })
		}
		return run(func() (string, error) { return "interrupted Grimm", s.interrupt(ctx, tx, sess) })
	case "/archive":
		target := ""
		if len(rest) > 0 && rest[0] != "all" {
			target = rest[0]
		}
		return run(func() (string, error) {
			names, err := s.archiveWorkers(ctx, tx, sess, target, "human")
			if len(names) == 0 {
				return "archived: (none)", err
			}
			return "archived: " + strings.Join(names, ", "), err
		})
	case "/new":
		return run(func() (string, error) { return "fresh manager conversation", s.newManager(ctx, tx, sess) })
	case "/cap":
		return run(func() (string, error) {
			n, err := strconv.Atoi(restText)
			if err != nil || n < 1 || n > MaxWorkersLimit {
				return "", badRequest("usage: /cap <1-%d>", MaxWorkersLimit)
			}
			if _, err := tx.Exec(ctx, `UPDATE grimm_sessions SET max_workers = $2 WHERE id = $1`, sess.ID, n); err != nil {
				return "", err
			}
			return fmt.Sprintf("worker cap set to %d", n), event(ctx, tx, sess, "human", "cap", "info", fmt.Sprintf("Set the worker cap to %d", n), nil)
		})
	case "/say":
		if len(rest) < 2 {
			return run(func() (string, error) { return "", badRequest("usage: /say <name> <message>") })
		}
		return run(func() (string, error) {
			return s.say(ctx, tx, sess, "human", rest[0], strings.TrimSpace(strings.TrimPrefix(restText, rest[0])))
		})
	case "/spawn":
		h, err := parseSpawn(rest)
		return runOr(run, err, func() (string, error) {
			w, _, err := s.hire(ctx, tx, sess, h, "human")
			return HiredLine(w), err
		})
	case "/fanout":
		n, review, task, err := parseFanout(rest)
		return runOr(run, err, func() (string, error) { return s.fanout(ctx, tx, sess, n, review, task, "human") })
	}
	return run(func() (string, error) { return "", badRequest("unknown command %s; /help lists them", cmd) })
}

func runOr(run func(func() (string, error)) (int64, error), parseErr error, fn func() (string, error)) (int64, error) {
	if parseErr != nil {
		return run(func() (string, error) { return "", parseErr })
	}
	return run(fn)
}

func parseSpawn(args []string) (Hire, error) {
	var h Hire
	var task []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "--role" || a == "--provider" || a == "--name") && i+1 < len(args):
			v := args[i+1]
			i++
			switch a {
			case "--role":
				h.Role = v
			case "--provider":
				h.Provider = v
			default:
				h.Name = v
			}
		default:
			task = append(task, a)
		}
	}
	// "/spawn athena write the docs": a short lowercase first word that is not the whole task is a name
	if h.Name == "" && len(task) > 1 && nameRe.MatchString(task[0]) && len(task[0]) <= 16 && !strings.Contains(task[0], "-") && isName(task[0]) {
		h.Name, task = task[0], task[1:]
	}
	h.Task = strings.Join(task, " ")
	if strings.TrimSpace(h.Task) == "" {
		return h, badRequest("usage: /spawn [name] [--role R] [--provider P] <task>")
	}
	return h, nil
}

// isName: a word from the name pool, so "/spawn athena …" names the worker and "/spawn write …" does not.
func isName(w string) bool {
	for _, n := range workerNames {
		if n == w {
			return true
		}
	}
	return false
}

func parseFanout(args []string) (int, bool, string, error) {
	usage := badRequest("usage: /fanout <N> [--review] <task>")
	if len(args) < 2 {
		return 0, false, "", usage
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > 10 {
		return 0, false, "", badRequest("fan out to 1 to 10 workers")
	}
	review := false
	var task []string
	for _, a := range args[1:] {
		if a == "--review" {
			review = true
			continue
		}
		task = append(task, a)
	}
	if len(task) == 0 {
		return 0, false, "", usage
	}
	return n, review, strings.Join(task, " "), nil
}

// fanout hires n workers on one task, each knowing the others, and optionally a reviewer.
func (s *Service) fanout(ctx context.Context, tx pgx.Tx, sess Session, n int, review bool, task, actor string) (string, error) {
	taken := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT name FROM grimm_workers WHERE session_id = $1`, sess.ID)
	if err != nil {
		return "", err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	for _, x := range names {
		taken[x] = true
	}
	var planned []string
	for range n + map[bool]int{true: 1}[review] {
		name := nextName(taken, "")
		taken[name] = true
		planned = append(planned, name)
	}
	var lines []string
	for i, name := range planned {
		role, t := "builder", fmt.Sprintf("Part %d of %d of a fan-out across %s. Split the work with your peers on the bus first (post your plan), then do your part.\n\n%s", i+1, n, strings.Join(planned, ", "), task)
		if review && i == len(planned)-1 {
			role, t = "reviewer", "Review the work of "+strings.Join(planned[:n], ", ")+" on this task when they post their results; report findings to grimm.\n\n"+task
		}
		w, _, err := s.hire(ctx, tx, sess, Hire{Role: role, Name: name, Task: t, Peers: planned}, actor)
		if err != nil {
			return strings.Join(append(lines, "stopped: "+err.Error()), "\n"), err
		}
		lines = append(lines, HiredLine(w))
	}
	return strings.Join(lines, "\n"), nil
}

func directiveAdded(seq int, text string, pushed []string) string {
	out := fmt.Sprintf("saved directive #%d: %s", seq, text)
	if len(pushed) > 0 {
		out += " (pushed to " + strings.Join(pushed, ", ") + ")"
	}
	return out
}

func directivesText(ctx context.Context, tx pgx.Tx, session string) (string, error) {
	ds, err := activeDirectives(ctx, tx, session)
	if err != nil || len(ds) == 0 {
		return "(no standing directives)", err
	}
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%d. %s\n", d.Seq, d.Text)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func roster(ctx context.Context, tx pgx.Tx, session string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT name, role, provider, model, login, status, agent_status, task, last_response
		FROM grimm_worker_status WHERE session_id = $1 ORDER BY status = 'archived', created_at`, session)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name, role, prov, model, login, status, agent, task, last string
		if err := rows.Scan(&name, &role, &prov, &model, &login, &status, &agent, &task, &last); err != nil {
			return "", err
		}
		n++
		fmt.Fprintf(&b, "%s (%s) %s/%s [%s] %s", name, role, prov, model, login, status)
		if agent != "" && agent != status {
			fmt.Fprintf(&b, " (agent %s)", agent)
		}
		fmt.Fprintf(&b, "\n  task: %s\n", cut(oneLine(task), 160))
		if last != "" {
			fmt.Fprintf(&b, "  last reply: %s\n", cut(oneLine(last), 240))
		}
	}
	if n == 0 {
		return "No workers yet.", rows.Err()
	}
	return strings.TrimRight(b.String(), "\n"), rows.Err()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func busText(ctx context.Context, tx pgx.Tx, session, forAgent string, n int) (string, error) {
	n = max(1, min(n, 500))
	rows, err := tx.Query(ctx, `SELECT * FROM (SELECT id, created_at, sender, recipient, kind, body FROM grimm_messages
		WHERE session_id = $1 AND channel = 'bus' AND ($2 = '' OR recipient IN ($2, 'all') OR sender = $2)
		ORDER BY id DESC LIMIT $3) x ORDER BY id`, session, forAgent, n)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var at time.Time
		var from, to, kind, body string
		if err := rows.Scan(&id, &at, &from, &to, &kind, &body); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "#%d %s %s -> %s [%s]\n%s\n\n", id, at.UTC().Format(time.RFC3339), from, to, kind, body)
	}
	if b.Len() == 0 {
		return "(the bus is empty)", rows.Err()
	}
	return strings.TrimRight(b.String(), "\n"), rows.Err()
}

// peek is the tail of a worker's transcript, a line an entry.
func peek(ctx context.Context, tx pgx.Tx, w Worker, n int) (string, error) {
	if w.AgentID == "" {
		return "(" + w.Name + " has not started yet)", nil
	}
	n = max(1, min(n, 400))
	rows, err := tx.Query(ctx, `SELECT * FROM (SELECT seq_start, kind, tool, status, text, item FROM transcript_entries
		WHERE host = $1 AND agent_id = $2 ORDER BY seq_start DESC LIMIT $3) x ORDER BY seq_start`, w.Host, w.AgentID, n)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var seq int
		var kind, tool, status, text string
		var item json.RawMessage
		if err := rows.Scan(&seq, &kind, &tool, &status, &text, &item); err != nil {
			return "", err
		}
		b.WriteString(EntryLine(kind, tool, status, text, item))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "(no activity yet)", rows.Err()
	}
	return strings.TrimRight(b.String(), "\n"), rows.Err()
}

// EntryLine is a transcript entry as one log line: [Tool] what it ran, or the words.
func EntryLine(kind, tool, status, text string, item json.RawMessage) string {
	switch kind {
	case "tool_call":
		var it struct {
			Detail struct {
				Command  string          `json:"command"`
				FilePath string          `json:"filePath"`
				Query    string          `json:"query"`
				Input    json.RawMessage `json:"input"`
			} `json:"detail"`
		}
		_ = json.Unmarshal(item, &it)
		what := it.Detail.Command
		if what == "" {
			what = it.Detail.FilePath
		}
		if what == "" {
			what = it.Detail.Query
		}
		if what == "" && len(it.Detail.Input) > 0 {
			what = string(it.Detail.Input)
		}
		line := "[" + tool + "] " + cut(oneLine(what), 300)
		if status == "failed" {
			line = "[Failed " + tool + "] " + cut(oneLine(what), 300)
		}
		return line
	case "reasoning":
		return "[Thinking] " + cut(oneLine(text), 300)
	case "user_message":
		return "[Message] " + cut(oneLine(text), 300)
	case "assistant_message":
		return cut(text, 2000)
	}
	return "[" + kind + "]"
}
