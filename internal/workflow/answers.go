package workflow

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Answer is a sent message, including sends made before the final checked answer.
type Answer struct {
	Channel   string    `json:"channel"`
	MessageID string    `json:"messageId"`
	ThreadID  string    `json:"threadId"`
	SentAt    time.Time `json:"sentAt"`
	Checked   bool      `json:"checked"`
}

func (a Answer) validate() error {
	if strings.TrimSpace(a.Channel) == "" || len(a.Channel) > 80 || strings.TrimSpace(a.MessageID) == "" || len(a.MessageID) > 500 || len(a.ThreadID) > 500 {
		return bad("answer needs channel, messageId and an optional sentAt; identifiers must fit their limits")
	}
	return nil
}

// RecordAnswer permits retries of an identical record, but never rewrites delivery history.
func (e *Engine) RecordAnswer(ctx context.Context, host, id string, a Answer) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	if err := a.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id=$1 AND r.host=$2 FOR UPDATE OF r", id, host))
		if err != nil {
			return err
		}
		var sentAt *time.Time
		if !a.SentAt.IsZero() {
			sentAt = &a.SentAt
		}
		tag, err := tx.Exec(ctx, `INSERT INTO workflow_run_answers (run_id,account_id,channel,message_id,thread_id,sent_at,checked)
   VALUES ($1,$2,$3,$4,$5,coalesce($6,now()),$7) ON CONFLICT (run_id,channel,message_id) DO NOTHING`, id, r.Account, a.Channel, a.MessageID, a.ThreadID, sentAt, a.Checked)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var same bool
		err = tx.QueryRow(ctx, `SELECT thread_id=$4 AND ($5::timestamptz IS NULL OR sent_at=$5) AND checked=$6 FROM workflow_run_answers WHERE run_id=$1 AND channel=$2 AND message_id=$3`, id, a.Channel, a.MessageID, a.ThreadID, sentAt, a.Checked).Scan(&same)
		if err == nil && !same {
			return bad("this message is already recorded with different delivery details")
		}
		return err
	})
}

// AnswerResolution settles a run's owed answer without a send of its own: Kind "steered" hands the
// request to the run SteeredTo (answered once that run, or the run it hands it on to, sends a
// checked answer at or after At), and "not_needed" says, with Reason, that no answer is due.
// Kind "legacy" exists only from migration 0050, never from an agent.
type AnswerResolution struct {
	Kind      string    `json:"kind"`
	SteeredTo string    `json:"steeredTo,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at,omitzero"`
}

const (
	ResolutionSteered   = "steered"
	ResolutionNotNeeded = "not_needed"
)

func (a AnswerResolution) validate(run string) error {
	if len(a.Reason) > 2000 {
		return bad("the reason must fit in 2000 bytes")
	}
	switch a.Kind {
	case ResolutionSteered:
		if !uuidRe.MatchString(a.SteeredTo) || a.SteeredTo == run {
			return bad("steered needs steeredTo: the UUID of the other run that took this request")
		}
	case ResolutionNotNeeded:
		if strings.TrimSpace(a.Reason) == "" || a.SteeredTo != "" {
			return bad("not_needed needs a reason (why no answer is due) and no steeredTo")
		}
	default:
		return bad("kind must be steered or not_needed")
	}
	return nil
}

// RecordAnswerResolution records how run id settled its answer. A run has at most one; an
// identical retry is accepted, a different one refused, and a steer may not close a loop.
func (e *Engine) RecordAnswerResolution(ctx context.Context, host, id string, a AnswerResolution) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	if err := a.validate(id); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id=$1 AND r.host=$2 FOR UPDATE OF r", id, host))
		if err != nil {
			return err
		}
		var to *string
		if a.Kind == ResolutionSteered {
			to = &a.SteeredTo
			var exists, loops bool
			// The run it went to must be this account's (any host), and must not lead back here.
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_runs WHERE id=$1 AND account_id=$2),
 EXISTS (WITH RECURSIVE chain (id, path) AS (SELECT $1::uuid, ARRAY[$1::uuid]
  UNION ALL SELECT s.steered_to, c.path || s.steered_to FROM chain c JOIN workflow_run_answer_resolutions s ON s.run_id=c.id AND s.kind='steered' WHERE s.steered_to <> ALL (c.path))
  SELECT 1 FROM chain WHERE id=$3::uuid)`, a.SteeredTo, r.Account, id).Scan(&exists, &loops)
			if err != nil {
				return err
			}
			if !exists {
				return bad("no run %s in this account to steer to", a.SteeredTo)
			}
			if loops {
				return bad("run %s already hands its request back to this run", a.SteeredTo)
			}
		}
		var at *time.Time
		if !a.At.IsZero() {
			at = &a.At
		}
		tag, err := tx.Exec(ctx, `INSERT INTO workflow_run_answer_resolutions (run_id,account_id,kind,steered_to,reason,at)
 VALUES ($1,$2,$3,$4,$5,coalesce($6,now())) ON CONFLICT (run_id) DO NOTHING`, id, r.Account, a.Kind, to, strings.TrimSpace(a.Reason), at)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var same bool
		err = tx.QueryRow(ctx, `SELECT kind=$2 AND steered_to IS NOT DISTINCT FROM $3::uuid AND reason=$4 AND ($5::timestamptz IS NULL OR at=$5)
 FROM workflow_run_answer_resolutions WHERE run_id=$1`, id, a.Kind, to, strings.TrimSpace(a.Reason), at).Scan(&same)
		if err == nil && !same {
			return bad("this run already records a different answer resolution")
		}
		return err
	})
}

// parseAnswerCLI reads `intelligence workflow answer`: a sent answer (--channel, --message-id, ...),
// or a resolution: --steered-to <run> [--reason] or --not-needed <reason>.
func parseAnswerCLI(args []string) (string, []byte, error) {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.String("run", "", "")
	var a Answer
	fs.StringVar(&a.Channel, "channel", "", "")
	fs.StringVar(&a.MessageID, "message-id", "", "")
	fs.StringVar(&a.ThreadID, "thread-id", "", "")
	sent := fs.String("sent-at", "", "")
	fs.BoolVar(&a.Checked, "checked", false, "")
	steeredTo := fs.String("steered-to", "", "")
	notNeeded := fs.String("not-needed", "", "")
	reason := fs.String("reason", "", "")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if !uuidRe.MatchString(*run) || fs.NArg() != 0 {
		return "", nil, bad("answer needs --run <UUID> and flags only")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var at time.Time
	var err error
	if *sent != "" {
		at, err = time.Parse(time.RFC3339Nano, *sent)
		if err != nil {
			at, err = mail.ParseDate(*sent)
		}
		if err != nil {
			return "", nil, err
		}
	}
	if set["steered-to"] || set["not-needed"] {
		if set["steered-to"] == set["not-needed"] || set["channel"] || set["message-id"] || set["thread-id"] || set["checked"] || (set["not-needed"] && set["reason"]) {
			return "", nil, bad("use one of: --steered-to <run> [--reason <text>] [--sent-at <handover time>], --not-needed <reason>, or a sent answer's flags")
		}
		res := AnswerResolution{Kind: ResolutionSteered, SteeredTo: *steeredTo, Reason: *reason, At: at}
		if set["not-needed"] {
			res = AnswerResolution{Kind: ResolutionNotNeeded, Reason: *notNeeded, At: at}
		}
		if err = res.validate(*run); err != nil {
			return "", nil, err
		}
		b, _ := json.Marshal(res)
		return "/api/workflow-runs/" + *run + "/answer-resolution", b, nil
	}
	if set["reason"] {
		return "", nil, bad("--reason goes with --steered-to")
	}
	a.SentAt = at
	if err = a.validate(); err != nil {
		return "", nil, err
	}
	b, _ := json.Marshal(a)
	return "/api/workflow-runs/" + *run + "/answers", b, nil
}
