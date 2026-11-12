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
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if !uuidRe.MatchString(*run) || fs.NArg() != 0 {
		return "", nil, bad("answer needs --run <UUID> and flags only")
	}
	var err error
	if *sent != "" {
		a.SentAt, err = time.Parse(time.RFC3339Nano, *sent)
		if err != nil {
			a.SentAt, err = mail.ParseDate(*sent)
		}
		if err != nil {
			return "", nil, err
		}
	}
	if err = a.validate(); err != nil {
		return "", nil, err
	}
	b, _ := json.Marshal(a)
	return "/api/workflow-runs/" + *run + "/answers", b, nil
}
