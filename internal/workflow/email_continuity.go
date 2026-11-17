package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/agenttext"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// EmailReplyRule also reaches already-saved email workflows and resumed agents.
const EmailReplyRule = `For follow-up email replies, compare your answer with earlier sent replies in the thread. If a number, recommendation or yes/no changes, the first sentence must say "Correction:" and explain why. Use "Done" or "Changed" only for an action actually taken outside the email (a booking, payment or message sent). For a hypothetical or recalculation, say "Nothing was changed". When asked "did you do it" or "do I need to do anything", the first words must be yes/no, answering that question directly; put any Correction: explanation immediately after that answer. Read the entire thread before answering. Answer this new email once; earlier sent answers do not count as delivery of this follow-up. Pass these rules to every worker.`

// ContinueEmailTx reserves a separate request/answer lifecycle with the same agent,
// on the earlier workflow version. Admission and startup use the ordinary engine paths.
func (e *Engine) ContinueEmailTx(ctx context.Context, tx pgx.Tx, account int64, previous, input string, requestedAt time.Time) (string, bool, error) {
	if err := agenttext.Validate(input); err != nil {
		return "", false, credentialRefusal(err)
	}
	if !uuidRe.MatchString(previous) {
		return "", false, ErrNotFound
	}
	if requestedAt.IsZero() {
		requestedAt = e.Now()
	}
	// Queue-before-run is also the lock order used by startup reconciliation.
	var host string
	if err := tx.QueryRow(ctx, "SELECT host FROM workflow_runs WHERE id=$1 AND account_id=$2", previous, account).Scan(&host); err != nil {
		return "", false, err
	}
	if err := ownsHost(ctx, tx, account, host); err != nil {
		return "", false, err
	}
	if err := lockQueue(ctx, tx, host); err != nil {
		return "", false, err
	}
	r, err := e.lockRun(ctx, tx, account, previous)
	if err != nil {
		return "", false, err
	}
	var finished *time.Time
	if err = tx.QueryRow(ctx, "SELECT finished_at FROM workflow_runs WHERE id=$1", r.ID).Scan(&finished); err != nil {
		return "", false, err
	}
	if finished == nil || !finished.After(e.Now().Add(-24*time.Hour)) || r.Orchestrator == "" || (r.Status != Succeeded && r.Status != Failed && r.Status != Canceled) {
		return "", false, nil
	}
	estimate, err := repositoryMemory(ctx, tx, account, r.Host, r.WorkflowID, r.Repo, true)
	if err != nil {
		return "", false, err
	}
	priority := r.Def.EffectivePriority()
	var why string
	if priority != PriorityImmediate {
		why, err = e.room(ctx, tx, r.Host, "", r.Repo, estimate)
		if err != nil {
			return "", false, err
		}
	}
	status := FindingProject
	if why != "" {
		status = Queued
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,repo,branch,workspace_id,orchestrator_id,continued_run_id,created_at,updated_at,
 requested_at,answer_expected,project_selected,estimated_memory_bytes,priority,queued_at_creation,queue_reason)
 SELECT account_id,workflow_id,version,host,$2,$3,$5,repo,branch,workspace_id,orchestrator_id,id,$4,$4,$6,true,true,$7,$8,$9,$10 FROM workflow_runs WHERE id=$1 RETURNING id::text`, r.ID, taskName(input), input, e.Now(), status, requestedAt, estimate, priority, why != "", why).Scan(&id)
	if err != nil {
		return "", false, err
	}
	next, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id=$1", id))
	if err != nil {
		return "", false, err
	}
	if err = e.event(ctx, tx, next, "started", nil, nil, input); err != nil {
		return "", false, err
	}
	if err = e.recordProject(ctx, tx, next, r.Repo); err != nil {
		return "", false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_run_events(run_id,account_id,at,kind,text,continued_run_id) VALUES($1,$2,$3,'email_continuation_requested',$4,$5)`, id, account, e.Now(), input, r.ID); err != nil {
		return "", false, err
	}
	if why != "" {
		err = e.event(ctx, tx, next, "queued", nil, nil, why)
	} else {
		err = e.launch(ctx, tx, next)
	}
	return id, true, err
}

func (e *Engine) startEmailContinuation(ctx context.Context, tx pgx.Tx, r run) error {
	var archive *string
	if err := tx.QueryRow(ctx, "SELECT archive_command::text FROM workflow_runs WHERE id=$1", r.ContinuedRun).Scan(&archive); err != nil {
		return err
	}
	// Use the same prompt builder as a fresh launch, including live-run handoffs,
	// subscription policy, credential checks and answer-recording instructions.
	raw, err := e.orchestratorStartArgs(ctx, tx, r)
	if err != nil {
		return err
	}
	var ws hostclient.CreateWorkspaceArgs
	if err = json.Unmarshal(raw, &ws); err != nil {
		return err
	}
	args, _ := json.Marshal(hostclient.ContinueEmailArgs{RunID: r.ID, AgentID: r.Orchestrator, AfterCommand: archive, Text: fmt.Sprintf("Continue your earlier email task in new run %s (earlier run %s is over). Use only the NEW run ID for all workflow commands and answer records. Keep your research and earlier answers. Earlier sends do not answer this new request.\n\n%s\n\n%s", r.ID, r.ContinuedRun, EmailReplyRule, ws.Agent.Prompt)})
	c, err := fleet.EnqueueTx(ctx, tx, r.Host, hostclient.ContinueEmailKind, args, replyTTL, e.Now())
	if err != nil {
		return credentialRefusal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE workflow_runs SET status='running',pending_command=$2,updated_at=$3 WHERE id=$1", r.ID, c.ID, e.Now()); err != nil {
		return err
	}
	if err = e.event(ctx, tx, r, "workspace", nil, nil, r.Orchestrator); err != nil {
		return err
	}
	r.Status = Running
	return e.refreshActivity(ctx, tx, r)
}

func (e *Engine) emailContinued(ctx context.Context, tx pgx.Tx, r run, c fleet.Command, res hostclient.CommandResult) error {
	if c.Status == hostclient.CommandSucceeded {
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET pending_command=NULL WHERE id=$1", r.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workflow_run_events(run_id,account_id,at,kind,text,continued_run_id) SELECT id,account_id,$2,'email_continued',orchestrator_id,continued_run_id FROM workflow_runs WHERE id=$1`, r.ID, e.Now())
		return err
	}
	// A late delivery result cannot undo a lifecycle decision already made by the agent.
	if r.Status == Succeeded || r.Status == Failed || r.Status == Canceled {
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET pending_command=NULL WHERE id=$1", r.ID); err != nil {
			return err
		}
		return e.event(ctx, tx, r, "error", nil, nil, "Late email continuation delivery result: "+c.Error)
	}
	if c.Status == hostclient.CommandFailed && res.AgentGone {
		// Additional emails are in r.Input and reach the replacement's first prompt.
		if _, err := tx.Exec(ctx, `UPDATE commands SET status='failed',error='Included in cold-start input',finished_at=$2 WHERE args->>'afterCommand'=$1 AND status='queued'`, c.ID, e.Now()); err != nil {
			return err
		}
		if err := e.event(ctx, tx, r, "email_cold_start", nil, nil, "Previous orchestrator is gone; earlier sent answers and run notes are in the input."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET orchestrator_id='',workspace_id='',pending_command=NULL,launch_command=NULL,launched_at=NULL WHERE id=$1`, r.ID); err != nil {
			return err
		}
		r.Orchestrator, r.WorkspaceID = "", ""
		var why string
		var err error
		if !r.skipsQueue() {
			why, err = e.room(ctx, tx, r.Host, r.ID, r.Repo, r.EstimatedMemory)
		}
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
	// An uncertain send must never launch a competing answering agent.
	if err := e.event(ctx, tx, r, "error", nil, nil, "Email continuation delivery uncertain: "+c.Error); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET status='failed',error=$2,pending_command=NULL,finished_at=$3,updated_at=$3 WHERE id=$1`, r.ID, c.Error, e.Now()); err != nil {
		return err
	}
	r.Status = Failed
	return e.refreshActivity(ctx, tx, r)
}
