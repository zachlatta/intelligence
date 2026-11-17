package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// EmailReplyRule also reaches already-saved email workflows and resumed agents.
const EmailReplyRule = `For follow-up email replies, compare your answer with earlier sent replies in the thread. If a number, recommendation or yes/no changes, the first sentence must say "Correction:" and explain why. Use "Done" or "Changed" only for an action actually taken outside the email (a booking, payment or message sent). For a hypothetical or recalculation, say "Nothing was changed". When asked "did you do it" or "do I need to do anything", the first words must be yes/no, answering that question directly; put any Correction: explanation immediately after that answer. Read the entire thread before answering. Answer this new email once; earlier sent answers do not count as delivery of this follow-up.`

// ContinueEmailTx reserves a separate run with the same agent, on the original version.
// The host verifies persistence; only its explicit agentGone result permits a cold start.
func (e *Engine) ContinueEmailTx(ctx context.Context, tx pgx.Tx, account int64, previous, input string) (string, bool, error) {
	r, err := e.lockRun(ctx, tx, account, previous)
	if err != nil {
		return "", false, err
	}
	var finished *time.Time
	var archive *string
	if err = tx.QueryRow(ctx, "SELECT finished_at,archive_command::text FROM workflow_runs WHERE id=$1", r.ID).Scan(&finished, &archive); err != nil {
		return "", false, err
	}
	if finished == nil || !finished.After(e.Now().Add(-24*time.Hour)) || r.Orchestrator == "" || (r.Status != Succeeded && r.Status != Failed && r.Status != Canceled) {
		return "", false, nil
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,repo,branch,workspace_id,orchestrator_id,continued_run_id,created_at,updated_at)
 SELECT account_id,workflow_id,version,host,$2,$3,'running',repo,branch,workspace_id,orchestrator_id,id,$4,$4 FROM workflow_runs WHERE id=$1 RETURNING id::text`, r.ID, taskName(input), input, e.Now()).Scan(&id)
	if err != nil {
		return "", false, err
	}
	next, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id=$1", id))
	if err != nil {
		return "", false, err
	}
	if err := e.event(ctx, tx, next, "started", nil, nil, input); err != nil {
		return "", false, err
	}
	// The run is already running with this workspace reserved. Use the ordinary
	// step-entry event so step durations include its restoration and work.
	if err := e.event(ctx, tx, next, "workspace", nil, nil, r.Orchestrator); err != nil {
		return "", false, err
	}
	d, err := e.fillModels(ctx, tx, next, 0)
	if err != nil {
		return "", false, err
	}
	args, _ := json.Marshal(hostclient.ContinueEmailArgs{AgentID: r.Orchestrator, AfterCommand: archive, Text: fmt.Sprintf("Continue your earlier email task in new run %s (earlier run %s is over). Use only the NEW run ID for all workflow commands. Keep your research and earlier answers.\n\n%s\n\n%s", id, r.ID, EmailReplyRule, OrchestratorPrompt(r.Name, d, next.vars()))})
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.ContinueEmailKind, args, replyTTL, e.Now())
	if err != nil {
		return "", false, err
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_runs SET pending_command=$2,launch_command=$2,launched_at=$3 WHERE id=$1", id, c.ID, e.Now())
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO workflow_run_events(run_id,account_id,at,kind,text,continued_run_id) VALUES($1,$2,$3,'email_continuation_requested',$4,$5)`, id, account, e.Now(), input, r.ID)
	}
	return id, true, err
}

func (e *Engine) emailContinued(ctx context.Context, tx pgx.Tx, r run, c fleet.Command, res hostclient.CommandResult) error {
	if c.Status == hostclient.CommandSucceeded {
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET pending_command=NULL WHERE id=$1`, r.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO workflow_run_events(run_id,account_id,at,kind,text,continued_run_id) SELECT id,account_id,$2,'email_continued',orchestrator_id,continued_run_id FROM workflow_runs WHERE id=$1`, r.ID, e.Now())
		return err
	}
	if c.Status == hostclient.CommandFailed && res.AgentGone {
		// These emails are already in r.Input and will reach the replacement's
		// first prompt; do not also send them to the missing agent.
		if _, err := tx.Exec(ctx, `UPDATE commands SET status='failed',error='Included in cold-start input',finished_at=$2 WHERE args->>'afterCommand'=$1 AND status='queued'`, c.ID, e.Now()); err != nil {
			return err
		}
		if err := e.event(ctx, tx, r, "email_cold_start", nil, nil, "Previous orchestrator is gone; earlier sent answers and run notes are in the input."); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET orchestrator_id='',workspace_id='',pending_command=NULL WHERE id=$1`, r.ID)
		if err != nil {
			return err
		}
		r.Orchestrator, r.WorkspaceID = "", ""
		why, err := e.queueReason(ctx, tx, r.Host)
		if err != nil {
			return err
		}
		if why != "" {
			if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET status='queued',queue_reason=$2 WHERE id=$1", r.ID, why); err != nil {
				return err
			}
			return e.event(ctx, tx, r, "queued", nil, nil, why)
		}
		return e.launch(ctx, tx, r)
	}
	// A transport error or expiry may have delivered the message. Never start a second agent.
	if err := e.event(ctx, tx, r, "error", nil, nil, "Email continuation delivery uncertain: "+c.Error); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE workflow_runs SET status='failed',error=$2,pending_command=NULL,finished_at=$3,updated_at=$3 WHERE id=$1`, r.ID, c.Error, e.Now())
	return err
}
