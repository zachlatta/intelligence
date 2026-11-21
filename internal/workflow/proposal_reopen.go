package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
)

// Reopen restores review state only. Applying the snapshot still needs approval.
func (e *Engine) Reopen(ctx context.Context, account int64, id string) error {
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		return e.reopenProposalTx(ctx, tx, account, id, "reviewer", nil)
	})
}

// The account lock precedes the proposal lock, just as for approval.
func (e *Engine) reopenProposalTx(ctx context.Context, tx pgx.Tx, account int64, id, actor string, question *int64) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	var status string
	var wid *string
	var before []byte
	var expired bool
	var rejected *time.Time
	err := tx.QueryRow(ctx, `SELECT status,workflow_id::text,before_state,expires_at<=$3,decided_at FROM workflow_proposals WHERE id=$1 AND account_id=$2 FOR UPDATE`, id, account, e.Now()).Scan(&status, &wid, &before, &expired, &rejected)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "rejected" || expired {
		return conflict("reopening requires a rejected, unexpired proposal")
	}
	var current, base *WorkflowState
	if wid != nil {
		current, err = workflowState(ctx, tx, account, *wid)
		if err != nil {
			return err
		}
	}
	if err = json.Unmarshal(before, &base); err != nil {
		return err
	}
	if !reflect.DeepEqual(current, base) {
		return conflict("proposal is stale; workflow changed")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_proposal_reopens(proposal_id,actor,question_id,rejected_at,reopened_at) VALUES($1,$2,$3,$4,$5)`, id, actor, question, rejected, e.Now()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE workflow_proposals SET status='pending',decided_at=NULL WHERE id=$1`, id)
	return err
}
