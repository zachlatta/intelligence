package workflow

// Parallel runs on one deliverable. On 2026-11-12 an email run and a voice-memo run both rebuilt
// and republished one public deck in place, two minutes apart; the later publish dropped the
// earlier one's slide. Earlier that evening three voice memos found the run building the deck and
// steered into it instead. This makes that the rule:
//
//   - an email or voice-memo run's orchestrator starts with the account's live runs in its prompt,
//     and is told to steer into one working on the same deliverable rather than build in parallel;
//   - a run claims what it will change or publish before building (claim). Another live run's
//     claim on the same thing, or on a folder that holds it, is refused, naming that run;
//   - steer hands the request to that run's orchestrator, records it as the steering run's
//     steered answer resolution (answers.go: answered only by what that run sends from the
//     handover on) and ends the steering run. Later email in the steered run's thread goes to the
//     run it steered into while that run is live (SteeredInto).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// DeliverableKey is the form a claim on s is kept and compared in. A link is lowercase in its
// scheme and host, has no query or fragment, and names a folder without /index.html or a trailing
// slash, so a site's folder, its index page and a cache-busted asset URL compare as they should.
// Anything else is a name (a work item), compared without case and runs of spaces.
func DeliverableKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 500 || strings.ContainsFunc(s, unicode.IsControl) {
		return "", bad("a deliverable is one line of 1–500 characters: its public link or folder, or a short name for the work item")
	}
	if u, err := url.Parse(s); err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Host != "" {
		p := path.Clean("/" + u.Path)
		p = strings.TrimSuffix(p, "/index.html")
		p = strings.TrimSuffix(p, "/")
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + p, nil
	}
	return strings.Join(strings.Fields(strings.ToLower(s)), " "), nil
}

// sameDeliverable says whether claims a and b (keys) cover one thing: equal, or one a folder of
// the other.
func sameDeliverable(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// LiveRun is another live run of the account, as an email or voice-memo run's orchestrator is
// shown it before building.
type LiveRun struct {
	ID, Workflow, Summary, Status, Orchestrator string
	Step                                        int
	Started                                     time.Time
	Claims                                      []string
	Emails                                      []string // for a run started by email: its threads' subjects and Gmail thread ids
}

const overStatuses = "('" + Succeeded + "','" + Failed + "','" + Canceled + "')"

// liveRuns is account's live runs other than run, oldest first, with their claims.
func liveRuns(ctx context.Context, tx pgx.Tx, account int64, run string) ([]LiveRun, error) {
	rows, err := tx.Query(ctx, `SELECT r.id::text, w.name, coalesce(nullif(r.task_summary, ''), r.title), r.status, r.orchestrator_id, r.step, r.created_at,
		coalesce((SELECT array_agg(c.deliverable ORDER BY c.claimed_at, c.deliverable) FROM workflow_run_claims c WHERE c.run_id = r.id), '{}'),
		coalesce((SELECT array_agg(t.line ORDER BY t.first) FROM (SELECT '“' || (array_agg(m.subject ORDER BY m.id))[1] || '” (thread ' || m.thread_id || ')' AS line, min(m.id) AS first
			FROM emails m WHERE m.run_id = r.id GROUP BY m.thread_id LIMIT 5) t), '{}')
		FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id
		WHERE r.account_id = $1 AND r.id::text <> $2 AND r.status NOT IN `+overStatuses+`
		ORDER BY r.created_at LIMIT 30`, account, run)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveRun, error) {
		var l LiveRun
		err := row.Scan(&l.ID, &l.Workflow, &l.Summary, &l.Status, &l.Orchestrator, &l.Step, &l.Started, &l.Claims, &l.Emails)
		return l, err
	})
}

// liveRunsSection is what an email or voice-memo run's orchestrator reads before its steps: the
// live runs, and to steer into one on the same deliverable or else claim before building.
func liveRunsSection(run string, live []LiveRun) string {
	var b strings.Builder
	b.WriteString("Before building anything, check whether a live run is already working on the same deliverable: the same work item " +
		"(a deck, report, page, site or document), the same public link or file, or the same email thread chain. ")
	if len(live) == 0 {
		b.WriteString("No other run is live right now on this account.\n\n")
	} else {
		b.WriteString("The runs live now on this account:\n\n")
		for _, l := range live {
			fmt.Fprintf(&b, "- %s · “%s” · %s · %s, step %d · started %s", l.ID, l.Workflow, oneLine(l.Summary), strings.ReplaceAll(l.Status, "_", " "),
				l.Step+1, l.Started.UTC().Format(time.RFC3339))
			if l.Orchestrator != "" {
				fmt.Fprintf(&b, " · orchestrator %s", l.Orchestrator)
			}
			if len(l.Claims) > 0 {
				fmt.Fprintf(&b, " · claims: %s", strings.Join(l.Claims, ", "))
			}
			if len(l.Emails) > 0 {
				fmt.Fprintf(&b, " · email: %s", oneLine(strings.Join(l.Emails, ", ")))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nWhen a live run may be on the same deliverable, read what it is doing (paseo logs <its orchestrator>) before deciding.\n\n")
	}
	fmt.Fprintf(&b, "If a live run is working on the same deliverable, do not start a parallel build: steer into it with\n\n"+
		"  intelligence workflow steer --run %[1]s --into <run id> --note <file>|- [--deliverable <link or name>]\n\n"+
		"The note says what this request adds. The server sends it with this run's whole request to that run's orchestrator, records the steer as this run's answer resolution and finishes this run; "+
		"that run then delivers and answers this request too, so this run sends nothing more. "+
		"Otherwise, before building, claim each deliverable this run will change or publish (its public link or folder, or a short name for the work item):\n\n"+
		"  intelligence workflow claim --run %[1]s <link or name>\n\n"+
		"Claim the public link (or folder) as soon as you know it, not only a name: two runs on one deck rarely pick the same name, but they share its link. "+
		"A refused claim names the live run that holds the deliverable: steer into that run instead.", run)
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// SharedDeliverableRule is appended to every step at runtime, like FileDeliveryRule, so saved
// workflows get it too: what a run does before changing or republishing something another run may
// also be changing.
const SharedDeliverableRule = "Shared deliverables: before changing or republishing anything another run may also touch (a public link, a deck, a site, a shared file), " +
	"claim it with intelligence workflow claim --run {{run}} <link or name>. A refused claim names the live run that holds it: steer into that run " +
	"(intelligence workflow steer --run {{run}} --into <run id> --note <file>) rather than building in parallel. " +
	"When replacing an existing files.zachlatta.com file, pass expected_version (the version you built on, as the file-share skill says) on every replace. " +
	"If the service refuses with a 409 or a version conflict, stop and never overwrite: re-read the current version, rebuild the change on it, and report the conflict. " +
	"Give every rebuilt asset whose content changed a new file name (a short content hash or version suffix, such as map-3f2a9c1.png), and update every reference to it, " +
	"so browser and CDN caches cannot show the old one; never reuse a published name for new content. Pass this rule to every worker."

// claim records that r is changing deliverable, unless another live run of the account holds it.
func (e *Engine) claim(ctx context.Context, tx pgx.Tx, r run, deliverable string) (string, error) {
	key, err := DeliverableKey(deliverable)
	if err != nil {
		return "", err
	}
	// one account's claims are decided one at a time, so two runs cannot both claim one deck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('workflow_run_claims:' || $1::bigint, 0))", r.Account); err != nil {
		return "", err
	}
	live, err := liveRuns(ctx, tx, r.Account, r.ID)
	if err != nil {
		return "", err
	}
	for _, l := range live {
		for _, c := range l.Claims {
			if sameDeliverable(c, key) {
				return "", conflict("Run %s (“%s”: %s) is already working on %s (its claim: %s). Do not build or publish it in parallel: "+
					"steer into that run with intelligence workflow steer --run %s --into %s --note <file>|- --deliverable %s",
					l.ID, l.Workflow, oneLine(l.Summary), key, c, r.ID, l.ID, key)
			}
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO workflow_run_claims (run_id, account_id, deliverable, claimed_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, r.ID, r.Account, key, e.Now())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() > 0 {
		if err := e.event(ctx, tx, r, "claim", ptr(r.Step), nil, key); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Claimed %s for this run until it ends: another live run's claim on it is now refused. "+
		"Publish it with the file-share expected_version guard (the version you built on) and stop on a stale-version refusal; "+
		"give rebuilt assets whose content changed new file names. Carry on with the step.", key), nil
}

// steer sends r's request, with note, to the orchestrator of live run m.Into (or, when that run
// steered on, the live run its chain ends in, which it leaves in m.Into), records the steer and
// leaves finishing r to the caller.
func (e *Engine) steer(ctx context.Context, tx pgx.Tx, r run, m *Move) error {
	into := strings.TrimSpace(m.Into)
	note := strings.TrimSpace(m.Note)
	switch {
	case !uuidRe.MatchString(into):
		return bad("steer needs --into <run id>: the live run working on the same deliverable")
	case into == r.ID:
		return bad("a run cannot steer into itself")
	case note == "" || len(note) > 20000:
		return bad("steer needs a note of 1 to 20000 bytes (--note <file>|-): what this request adds to that run's work")
	}
	key := ""
	if strings.TrimSpace(m.Deliverable) != "" {
		var err error
		if key, err = DeliverableKey(m.Deliverable); err != nil {
			return err
		}
	}
	// a run steered into that has itself steered on: its work lives at the end of the chain
	if end, _, ok, err := e.SteeredInto(ctx, tx, r.Account, into); err != nil {
		return err
	} else if ok {
		into = end
	}
	if into == r.ID {
		return bad("that run steered into this one: carry on with this run")
	}
	m.Into = into
	about := "the same deliverable"
	if key != "" {
		about = key
	}
	what := fmt.Sprintf("Run %s of “%s” steered into this run, because it is about %s", r.ID, r.Name, about)
	text := note + "\n\nIts whole request:\n\n" + strings.TrimSpace(r.Input) + "\n\n" +
		"This run now owns that request: fold it into your work rather than leaving it to a parallel build, deliver it with yours, " +
		"and answer its requester too (in its email thread when it came by email). " +
		"Publish with the file-share expected_version guard, and give rebuilt assets whose content changed new file names."
	if err := e.MessageTx(ctx, tx, r.Account, into, what, text); err != nil {
		var ref *Refusal
		switch {
		case errors.Is(err, ErrNotFound):
			return bad("run %s is not a run of this account", into)
		case errors.As(err, &ref):
			return conflict("cannot steer into run %s: %s. If it is over, carry on with this run (claim, then build); if it is starting, try again in a minute.", into, ref.Msg)
		}
		return err
	}
	// only what that run sends from now on answers this request (workflow_run_answer_status)
	reason := note
	if key != "" {
		reason = key + ": " + note
	}
	if err := recordResolution(ctx, tx, r, AnswerResolution{Kind: ResolutionSteered, SteeredTo: into, Reason: truncate(reason, 2000), At: e.Now()}); err != nil {
		return err
	}
	// the run steered into records it as the message MessageTx sent it
	return e.event(ctx, tx, r, "steer", ptr(r.Step), nil, "Steered into run "+into+": "+note)
}

// SteeredInto follows run's steers (its steered answer resolutions) to the run its work now lives
// in, and that run's workflow, while that run is live: a later email in run's thread, or a steer
// into run, belongs there.
func (e *Engine) SteeredInto(ctx context.Context, tx pgx.Tx, account int64, run string) (into, workflowID string, ok bool, err error) {
	at, seen := run, map[string]bool{run: true}
	for {
		var next, wf, status string
		err := tx.QueryRow(ctx, `SELECT s.steered_to::text, r.workflow_id::text, r.status FROM workflow_run_answer_resolutions s JOIN workflow_runs r ON r.id = s.steered_to
			WHERE s.run_id::text = $1 AND s.account_id = $2 AND s.kind = 'steered'`, at, account).Scan(&next, &wf, &status)
		if errors.Is(err, pgx.ErrNoRows) || seen[next] {
			break
		}
		if err != nil {
			return "", "", false, err
		}
		seen[next] = true
		at, workflowID = next, wf
		ok = status != Succeeded && status != Failed && status != Canceled
	}
	if at == run || !ok {
		return "", "", false, nil
	}
	return at, workflowID, true, nil
}

// truncate is s cut to at most n bytes, on a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// noteUnansweredSteers records, on each run steered to r whose request is still unanswered, that r
// ended (as status) without answering it; such a run is unanswered again (workflow_runs_unanswered).
func (e *Engine) noteUnansweredSteers(ctx context.Context, tx pgx.Tx, r run, status string) error {
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text)
		SELECT st.id, st.account_id, $2, 'steer_unanswered', w.step, $3
		FROM workflow_run_answer_status st JOIN workflow_runs w ON w.id = st.id
		WHERE st.resolution = 'steered' AND st.steered_to = $1 AND st.answered_at IS NULL AND st.answer_expected
		AND NOT EXISTS (SELECT 1 FROM workflow_run_answer_resolutions s WHERE s.run_id = $1 AND s.kind = 'steered')`,
		r.ID, e.Now(), "Run "+r.ID+", which this run steered into, ended ("+status+") without a checked answer to this request: it is unanswered again.")
	return err
}
