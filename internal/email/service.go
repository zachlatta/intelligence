package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// Service connects each account's Gmail mailbox, reads what arrives in it, and does what the
// account's email triggers say: start a run, add to the run of the email's thread, or answer the
// question that run waits on. Each email is kept in the emails table with what it did and why.
type Service struct {
	DB     *pgxpool.Pool // as the owner
	Engine *workflow.Engine
	Google *Google
	Key    *Keyring
	Now    func() time.Time

	mu        sync.Mutex
	mailboxes map[int64]*Mailbox // each account's, so its access token is reused
}

// Interval is how often each mailbox is read.
const Interval = time.Second

// backfill is what is read in when a mailbox is connected, for the trigger editor's preview.
const (
	backfillQuery = "newer_than:30d -in:sent -in:drafts -in:spam -in:trash"
	backfillMax   = 200
)

// Refusal is a request the caller can fix.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Start is where to send account's browser to connect a mailbox, and whether Google will send it
// to a localhost page whose address must be pasted back (a Desktop client).
func (s *Service) Start(account int64, base string) (signIn string, paste bool) {
	state := s.Key.State(account, s.now())
	return s.Google.SignInURL(state, Challenge(s.Key.Verifier(state)), s.Google.Redirect(base)), s.Google.Installed
}

// Connect finishes account's sign-in with Google's code and state: it keeps the mailbox, from now
// on. A mailbox connected again keeps the account's own addresses.
func (s *Service) Connect(ctx context.Context, account int64, code, state, base string) (string, error) {
	acct, err := s.Key.CheckState(state, s.now())
	if err != nil {
		return "", &Refusal{err.Error()}
	}
	if acct != account {
		return "", &Refusal{"this sign-in was started by another account: start again"}
	}
	refresh, err := s.Google.Exchange(ctx, code, s.Key.Verifier(state), s.Google.Redirect(base))
	if err != nil {
		return "", &Refusal{err.Error()}
	}
	mb := s.Google.Mailbox(refresh)
	address, history, err := mb.Profile(ctx)
	if err != nil {
		return "", err
	}
	sealed, err := s.Key.Seal(refresh)
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO gmail_connections (account_id, address, token, history_id, connected_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_id) DO UPDATE SET address = $2, token = $3, history_id = $4, connected_at = $5, backfilled = false,
			error = '', error_at = NULL, backfill_ids = NULL,
			owners = CASE WHEN gmail_connections.address = $2 THEN gmail_connections.owners ELSE '{}' END`,
		account, address, sealed, history, s.now())
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.mailboxes == nil {
		s.mailboxes = map[int64]*Mailbox{}
	}
	s.mailboxes[account] = mb
	s.mu.Unlock()
	return address, nil
}

// SetOwners keeps the account holder's own addresses: only their verified email answers a run's
// question, and anyone else's reaches a run as information, not instructions.
func (s *Service) SetOwners(ctx context.Context, account int64, owners []string) error {
	var list []string
	for _, o := range owners {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" || slices.Contains(list, o) {
			continue
		}
		if a, err := mail.ParseAddress(o); err != nil || a.Address != o {
			return &Refusal{fmt.Sprintf("%q is not an email address", o)}
		}
		list = append(list, o)
	}
	if list == nil {
		list = []string{}
	}
	tag, err := s.DB.Exec(ctx, "UPDATE gmail_connections SET owners = $2 WHERE account_id = $1", account, list)
	if err == nil && tag.RowsAffected() == 0 {
		return &Refusal{"no mailbox is connected"}
	}
	return err
}

// Disconnect forgets account's mailbox (its emails stay, with what they did).
func (s *Service) Disconnect(ctx context.Context, account int64) error {
	s.mu.Lock()
	delete(s.mailboxes, account)
	s.mu.Unlock()
	_, err := s.DB.Exec(ctx, "DELETE FROM gmail_connections WHERE account_id = $1", account)
	return err
}

// Run reads every mailbox every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	ctx = context.WithValue(ctx, intervalKey{}, interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	// Keep scheduling while a worker paces a backfill. Each account has at most
	// one active job; the database row lock also protects across server instances.
	done := make(chan int64, 4)
	active := map[int64]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	limit := min(4, max(1, int(s.DB.Config().MaxConns)/2))
	schedule := func() {
		rows, err := s.DB.Query(ctx, "SELECT account_id FROM gmail_connections WHERE next_poll_at <= $1 ORDER BY next_poll_at, account_id", s.now())
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("email: %v", err)
			}
			return
		}
		accounts, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("email: %v", err)
			}
			return
		}
		for _, a := range accounts {
			if len(active) >= limit {
				break
			}
			if active[a] {
				continue
			}
			active[a] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Poll(ctx, a); err != nil && ctx.Err() == nil {
					log.Printf("email account %d: %v", a, err)
				}
				done <- a
			}()
		}
	}
	schedule()
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-done:
			delete(active, a)
		case <-t.C:
			schedule()
		}
	}
}

func (s *Service) mailbox(account int64, refresh string) *Mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mailboxes == nil {
		s.mailboxes = map[int64]*Mailbox{}
	}
	if mb := s.mailboxes[account]; mb != nil && mb.refresh == refresh {
		return mb
	}
	mb := s.Google.Mailbox(refresh)
	s.mailboxes[account] = mb
	return mb
}

// Poll reads what arrived in account's mailbox since it was last read, and does what each email
// says. One server reads a mailbox at a time: the others skip it while it is locked.
func (s *Service) Poll(ctx context.Context, account int64) error {
	return pgx.BeginFunc(ctx, s.DB, func(lock pgx.Tx) error {
		var owners []string
		var token []byte
		var history string
		var backfilled bool
		var pending []string
		var connected, nextPoll, nextRequest time.Time
		var failures int
		err := lock.QueryRow(ctx, `SELECT owners, token, history_id, backfilled, connected_at, next_poll_at, next_request_at, retry_count, backfill_ids FROM gmail_connections WHERE account_id = $1
			FOR UPDATE SKIP LOCKED`, account).Scan(&owners, &token, &history, &backfilled, &connected, &nextPoll, &nextRequest, &failures, &pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if s.now().Before(nextPoll) {
			return nil
		}
		started := s.now()
		pace := &pacer{Next: nextRequest}
		perr := s.read(context.WithValue(ctx, paceKey{}, pace), account, owners, token, connected, &history, &backfilled, &pending)
		every := Interval
		if d, ok := ctx.Value(intervalKey{}).(time.Duration); ok {
			every = d
		}
		// Leave scheduling slack relative to the actual start, independent of wall-clock
		// boundaries. A slightly late worker must still be due at the next timer tick.
		// Error retries below use their full delay, without this slack.
		nextPoll = started.Add(every - every/10)
		if perr != nil {
			// Back off on every failure, honouring a longer Retry-After: network, token and
			// local failures must not become one-second retry storms either.
			var requested time.Duration
			if retry := (*retryError)(nil); errors.As(perr, &retry) {
				requested = retry.Delay
			}
			nextPoll = s.now().Add(max(every, retryDelay(failures, requested)))
			failures = min(failures+1, 7)
		} else {
			failures = 0
		}
		if _, err = lock.Exec(ctx, "UPDATE gmail_connections SET next_poll_at=$2, next_request_at=$3, retry_count=$4, backfill_ids=$5 WHERE account_id=$1", account, nextPoll, pace.Next, failures, pending); err != nil {
			return err
		}
		if perr != nil {
			_, err = lock.Exec(ctx, "UPDATE gmail_connections SET history_id = $2, backfilled = $3, error = $4, error_at = $5 WHERE account_id = $1",
				account, history, backfilled, perr.Error(), s.now())
			if err != nil {
				return err
			}
			return nil // the error is kept for the page; the next read tries again
		}
		_, err = lock.Exec(ctx, `UPDATE gmail_connections SET history_id = $2, backfilled = $3, checked_at = $4, error = '', error_at = NULL
			WHERE account_id = $1`, account, history, backfilled, s.now())
		return err
	})
}

func (s *Service) read(ctx context.Context, account int64, owners []string, token []byte, connected time.Time, history *string, backfilled *bool, pending *[]string) error {
	refresh, err := s.Key.Open(token)
	if err != nil {
		return err
	}
	mb := s.mailbox(account, refresh)
	ids, latest, err := mb.Added(ctx, *history)
	if errors.Is(err, ErrHistoryGone) {
		// unread for over a week: start again from now, and say so
		_, now, perr := mb.Profile(ctx)
		if perr != nil {
			return perr
		}
		*history = now
		return errors.New("the mailbox was not read for too long: email from before " + s.now().Format(time.DateTime) + " was skipped")
	}
	if err != nil {
		return err
	}
	// A failed batch leaves the history cursor unchanged. Messages already kept
	// have also had their routing decision committed, so retry only unsaved IDs
	// rather than paying another full-message fetch for each successful predecessor.
	stored := map[string]bool{}
	if len(ids) > 0 {
		rows, err := s.DB.Query(ctx, "SELECT gmail_id FROM emails WHERE account_id = $1 AND gmail_id = ANY($2::text[])", account, ids)
		if err != nil {
			return err
		}
		known, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range known {
			stored[id] = true
		}
	}
	for _, id := range ids {
		if stored[id] {
			continue
		}
		g, err := mb.Message(ctx, id)
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				continue // deleted since it arrived
			}
			return err
		}
		if err := s.keep(ctx, account, g, func(tx pgx.Tx, m workflow.Mail) (decision, error) {
			return s.route(ctx, tx, account, owners, g, m)
		}); err != nil {
			return err
		}
	}
	*history = latest
	// then what came before it was connected: new mail first, and none of it, which the next
	// read finds in the history, is taken as before
	if !*backfilled {
		if *pending == nil {
			ids, err := mb.Search(ctx, backfillQuery, backfillMax)
			if err != nil {
				return err
			}
			*pending = orEmpty(ids)
		}
		// Read one preview message per poll, so pacing a large backfill does not
		// keep new arrivals waiting for several minutes. Resume the stored queue.
		if len(*pending) > 0 {
			g, err := mb.Message(ctx, (*pending)[0])
			if err != nil && !strings.Contains(err.Error(), "404") {
				return err
			}
			if err == nil && g.Received().Before(connected) {
				if err := s.keep(ctx, account, g, func(pgx.Tx, workflow.Mail) (decision, error) {
					return decision{Decision: "before", Reason: "it came before the mailbox was connected"}, nil
				}); err != nil {
					return err
				}
			}
			*pending = (*pending)[1:]
		}
		*backfilled = len(*pending) == 0
	}
	return nil
}

// decision is what an email did.
type decision struct {
	Decision, Reason, Workflow, Run string
}

// keep stores g, if it is new, and what decide says it does, in one transaction: an email does
// what it does once. The mailbox's own mail, spam and trash are not kept.
func (s *Service) keep(ctx context.Context, account int64, g GmailMessage, decide func(pgx.Tx, workflow.Mail) (decision, error)) error {
	if g.Ours() || g.Junk() {
		return nil
	}
	m := g.Mail()
	_, auth := g.Verify()
	received := g.Received()
	if received.IsZero() {
		received = s.now()
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO emails (account_id, gmail_id, thread_id, received_at, from_name, from_address, to_addresses,
			subject, body, attachments, verified, auth, bulk, decision, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'ignored', $14)
			ON CONFLICT (account_id, gmail_id) DO NOTHING RETURNING id`,
			account, g.ID, g.ThreadID, received, m.FromName, m.From, orEmpty(m.To), m.Subject, m.Text, orEmpty(m.Attachments),
			m.Verified, auth, g.Bulk(), s.now()).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already kept, and done
		}
		if err != nil {
			return err
		}
		d, err := decide(tx, m)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE emails SET decision = $2, reason = $3, workflow_id = nullif($4, '')::uuid, run_id = nullif($5, '')::uuid
			WHERE id = $1`, id, d.Decision, d.Reason, d.Workflow, d.Run)
		return err
	})
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// route decides what m does: in the thread of a run, what that workflow says of replies;
// otherwise, it starts the workflow whose trigger matches it best, if any.
func (s *Service) route(ctx context.Context, tx pgx.Tx, account int64, owners []string, g GmailMessage, m workflow.Mail) (decision, error) {
	fromZach := m.Verified && slices.Contains(owners, m.From)
	var runID, wfID string
	err := tx.QueryRow(ctx, `SELECT run_id::text, workflow_id::text FROM emails
		WHERE account_id = $1 AND thread_id = $2 AND run_id IS NOT NULL AND workflow_id IS NOT NULL AND gmail_id <> $3
		ORDER BY id DESC LIMIT 1`, account, m.ThreadID, m.MessageID).Scan(&runID, &wfID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decision{}, err
	}
	if runID != "" {
		status, _, err := s.Engine.RunStatus(ctx, tx, account, runID)
		if err == nil {
			return s.reply(ctx, tx, account, fromZach, m, runID, wfID, status)
		}
		if !errors.Is(err, workflow.ErrNotFound) {
			return decision{}, err
		}
	}
	if g.Bulk() {
		return decision{Decision: "ignored", Reason: "a mailing list or newsletter, which never starts a run"}, nil
	}
	wfs, err := workflow.EmailWorkflows(ctx, tx, account)
	if err != nil {
		return decision{}, err
	}
	var best *workflow.EmailWorkflow
	score, why := -1, "no workflow's email trigger matches it"
	for i, w := range wfs {
		ok, reason := w.Def.Email.Match(m)
		if !ok {
			if strings.HasPrefix(reason, "forged sender") {
				why = reason
			}
			continue
		}
		if sc := w.Def.Email.Score(m); sc > score {
			best, score = &wfs[i], sc
		}
	}
	if best == nil {
		return decision{Decision: "ignored", Reason: why}, nil
	}
	return s.start(ctx, tx, account, best.ID, best.Name, m.Input(fromZach), "started")
}

// reply is what m does in the thread of run (of workflow wf), whose status is status.
func (s *Service) reply(ctx context.Context, tx pgx.Tx, account int64, fromZach bool, m workflow.Mail, run, wf, status string) (decision, error) {
	d := decision{Workflow: wf, Run: run}
	if !m.Verified {
		d.Decision, d.Reason = "ignored", "forged sender: Gmail could not verify that "+m.From+" sent it"
		return d, nil
	}
	replies, name := workflow.RepliesToRun, "the workflow"
	var def workflow.Definition
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT w.name, v.definition FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL`, wf, account).Scan(&name, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		d.Decision, d.Reason = "ignored", "the workflow of this thread's run is gone (deleted)"
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return d, err
	}
	if def.Email != nil {
		replies = def.Email.Replies
	}
	input := m.Input(fromZach)
	switch {
	case replies == workflow.RepliesIgnore:
		d.Decision, d.Reason = "ignored", "“"+name+"” ignores replies in its runs' threads"
		return d, nil
	case replies == workflow.RepliesNewRun:
		return s.start(ctx, tx, account, wf, name, input, "started")
	}
	switch status {
	case workflow.Succeeded, workflow.Failed, workflow.Canceled:
		return s.start(ctx, tx, account, wf, name, input+fmt.Sprintf("\n\nIt replies in the thread of an earlier run of this workflow, %s, which is over (%s).", run, status), "follow_up")
	case workflow.Waiting:
		if fromZach {
			text := strings.TrimSpace(m.Text)
			if text == "" {
				text = m.Subject
			}
			return d.or(try(ctx, tx, "answered", func(sp pgx.Tx) error { return s.Engine.ReplyTx(ctx, sp, account, run, text) }))
		}
	}
	return d.or(try(ctx, tx, "added", func(sp pgx.Tx) error {
		return s.Engine.MessageTx(ctx, sp, account, run, "A new email in this run's thread, from "+m.Who(), input)
	}))
}

// start starts a run of workflow wf, recorded as kind; a run the workflow cannot start is
// recorded as ignored, with why.
func (s *Service) start(ctx context.Context, tx pgx.Tx, account int64, wf, name, input, kind string) (decision, error) {
	d := decision{Workflow: wf}
	return d.or(try(ctx, tx, kind, func(sp pgx.Tx) error {
		id, err := s.Engine.StartRunTx(ctx, sp, account, wf, input)
		if err != nil {
			return fmt.Errorf("could not start “%s”: %w", name, err)
		}
		d.Run = id
		return nil
	}))
}

// or is d with what try decided: its kind when done, else ignored with the refusal.
func (d *decision) or(kind, refused string, err error) (decision, error) {
	if refused != "" {
		d.Decision, d.Reason = "ignored", refused
	} else {
		d.Decision = kind
	}
	return *d, err
}

// try does f in a savepoint, and returns kind; if f is refused (a workflow refusal), the
// savepoint is undone and refused says why.
func try(ctx context.Context, tx pgx.Tx, kind string, f func(pgx.Tx) error) (string, string, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return kind, "", err
	}
	if err := f(sp); err != nil {
		_ = sp.Rollback(ctx)
		var ref *workflow.Refusal
		if errors.As(err, &ref) || errors.Is(err, workflow.ErrNotFound) {
			return kind, err.Error(), nil
		}
		return kind, "", err
	}
	return kind, "", sp.Commit(ctx)
}

// TrimQuoted cuts a reply's quoted history: from a line "On … wrote:" (which mail apps may wrap
// over a few lines) or "-----Original Message-----", and trailing "> " lines.
func TrimQuoted(text string) string {
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "-----Original Message-----" ||
			(strings.HasPrefix(t, "On ") && strings.Contains(strings.Join(lines[i:min(i+3, len(lines))], " "), "wrote:")) {
			lines = lines[:i]
			break
		}
	}
	for len(lines) > 0 && (strings.TrimSpace(lines[len(lines)-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), ">")) {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
