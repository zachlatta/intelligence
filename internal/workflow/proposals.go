package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

type ProposalRequest struct {
	Input       string         `json:"input,omitempty"`
	Operation   string         `json:"operation"`
	WorkflowID  string         `json:"workflowId,omitempty"`
	BaseVersion int            `json:"baseVersion,omitempty"`
	Summary     string         `json:"summary"`
	Workflow    *WorkflowState `json:"workflow,omitempty"`
	ReplacesID  string         `json:"replacesId,omitempty"`
	ProposedBy  string         `json:"proposedBy,omitempty"`
	RunID       string         `json:"runId,omitempty"`
}

// Withdraw only removes the caller's pending proposal; it never edits workflows.
func (e *Engine) Withdraw(ctx context.Context, account int64, proposer, id, reason string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 {
		return bad("withdraw requires a reason (up to 2000 characters)")
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		return e.withdrawTx(ctx, tx, account, proposer, id, reason)
	})
}

func (e *Engine) withdrawTx(ctx context.Context, tx pgx.Tx, account int64, proposer, id, reason string) error {
	var by, status string
	var expired bool
	err := tx.QueryRow(ctx, "SELECT proposer,status,expires_at<=$3 FROM workflow_proposals WHERE id=$1 AND account_id=$2 FOR UPDATE", id, account, e.Now()).Scan(&by, &status, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if by != proposer {
		return ErrNotFound
	}
	if status != "pending" || expired {
		return conflict("withdrawal or replacement requires your own pending, unexpired proposal")
	}
	_, err = tx.Exec(ctx, "UPDATE workflow_proposals SET status='withdrawn',withdraw_reason=$2,decided_at=$3 WHERE id=$1", id, reason, e.Now())
	return err
}

// Propose records a reviewed workflow edit or run request without applying it.
func (e *Engine) Propose(ctx context.Context, account int64, proposer string, r ProposalRequest) (string, error) {
	if r.ReplacesID != "" && !uuidRe.MatchString(r.ReplacesID) {
		return "", bad("replacesId must be a proposal UUID")
	}
	if len(r.ProposedBy) > 500 || len(r.RunID) > 200 {
		return "", bad("proposedBy or runId is too long")
	}
	if strings.TrimSpace(r.Summary) == "" || len(r.Summary) > 2000 {
		return "", bad("summary is required (up to 2000 characters)")
	}
	switch r.Operation {
	case "create":
		if r.WorkflowID != "" || r.BaseVersion != 0 {
			return "", bad("create takes no workflowId or baseVersion")
		}
	case "update", "delete":
		if !uuidRe.MatchString(r.WorkflowID) || r.BaseVersion < 1 {
			return "", bad("workflowId and baseVersion are required")
		}
	case "run": // baseVersion is an optional drift guard; the current state is snapshotted.
		if !uuidRe.MatchString(r.WorkflowID) || r.BaseVersion < 0 {
			return "", bad("run requires workflowId; baseVersion is optional")
		}
		if strings.TrimSpace(r.Input) == "" || len(r.Input) > 1<<20 {
			return "", bad("run input must contain 1 to 1048576 bytes")
		}
	default:
		return "", bad("operation must be create, update, delete or run")
	}
	if r.Operation != "run" && r.Input != "" {
		return "", bad("input is only supported for run proposals")
	}
	if r.Operation == "delete" || r.Operation == "run" {
		if r.Workflow != nil {
			return "", bad("%s takes no workflow", r.Operation)
		}
	} else {
		if r.Workflow == nil {
			return "", bad("workflow is required")
		}
		name, err := checkName(r.Workflow.Name)
		if err != nil {
			return "", err
		}
		r.Workflow.Name = name
		r.Workflow.Description = strings.TrimSpace(r.Workflow.Description)
		r.Workflow.Definition.defaultPriority()
		if r.Workflow.Version != 0 || r.Workflow.IsDefault || r.Workflow.Deleted {
			return "", bad("workflow takes only name, description and definition")
		}
		if err := r.Workflow.Definition.Validate(); err != nil {
			return "", bad("%s", err)
		}
	}
	var id string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		// Use the same lock order as decisions, including replacement withdrawal.
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		var before *WorkflowState
		if r.Operation != "create" {
			var err error
			before, err = workflowState(ctx, tx, account, r.WorkflowID)
			if err != nil {
				return err
			}
			if before.Deleted || (r.BaseVersion != 0 && before.Version != r.BaseVersion) {
				return conflict("workflow changed; read its latest state and propose again")
			}
		}
		if r.Workflow != nil {
			if err := validateModelTokens(ctx, tx, account, r.Workflow.Definition); err != nil {
				return err
			}
			if err := ownsHost(ctx, tx, account, r.Workflow.Definition.Host); err != nil {
				return err
			}
			if err := uniqueEmailTrigger(ctx, tx, account, r.WorkflowID, r.Workflow.Definition); err != nil {
				return err
			}
		}
		// Store the exact target, including automatic revision/default changes.
		switch r.Operation {
		case "create":
			r.Workflow.Version = 1
		case "update":
			r.Workflow.Version = before.Version + 1
			r.Workflow.IsDefault = before.IsDefault && r.Workflow.Definition.Trigger == TriggerTask
		case "delete":
			copy := *before
			r.Workflow = &copy
			r.Workflow.Deleted, r.Workflow.IsDefault = true, false
		case "run": // The reviewed workflow is unchanged; approval refuses drift from it.
			if before.Definition.Host == "" {
				return bad("workflow must name a host before proposing a run")
			}
			if err := ownsHost(ctx, tx, account, before.Definition.Host); err != nil {
				return err
			}
			copy := *before
			r.Workflow = &copy
		}
		b, _ := json.Marshal(before)
		p, _ := json.Marshal(r.Workflow)
		if r.ReplacesID != "" {
			if err := e.withdrawTx(ctx, tx, account, proposer, r.ReplacesID, "Replaced: "+r.Summary); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `INSERT INTO workflow_proposals(account_id,proposer,operation,workflow_id,summary,before_state,proposed_state,created_at,expires_at,proposed_by,run_id,replaces_id,input)
   VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8::timestamptz,$8::timestamptz+interval '7 days',$9,$10,NULLIF($11,'')::uuid,$12) RETURNING id::text`, account, proposer, r.Operation, r.WorkflowID, r.Summary, b, p, e.Now(), r.ProposedBy, r.RunID, r.ReplacesID, r.Input).Scan(&id)
	})
	return id, err
}

// Decide serializes on the proposal and workflow. The before/after comparison includes
// metadata and default state as well as the definition, so undo cannot overwrite later work.
func (e *Engine) Decide(ctx context.Context, account int64, id, action string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	var refusal error
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		// Account lock serializes proposal creates/restores and default restoration.
		if err := lockWorkflowAccount(ctx, tx, account); err != nil {
			return err
		}
		var status, operation, input string
		var wid *string
		var before, proposed, after []byte
		var expired bool
		err := tx.QueryRow(ctx, `SELECT status,workflow_id::text,before_state,proposed_state,after_state,expires_at <= $3,operation,input
   FROM workflow_proposals WHERE id=$1 AND account_id=$2 FOR UPDATE`, id, account, e.Now()).Scan(&status, &wid, &before, &proposed, &after, &expired, &operation, &input)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		set := func(s string) error {
			_, err := tx.Exec(ctx, "UPDATE workflow_proposals SET status=$2,decided_at=$3 WHERE id=$1", id, s, e.Now())
			return err
		}
		if action == "undo" {
			if operation == "run" {
				return conflict("run proposals cannot be undone; cancel the run separately")
			}
			if status != "applied" {
				return conflict("only an applied proposal can be undone")
			}
		} else {
			if status != "pending" {
				return conflict("proposal is %s", status)
			}
			if expired {
				refusal = conflict("proposal expired; propose again")
				return set("expired")
			}
			if action == "reject" {
				return set("rejected")
			}
			if action != "approve" {
				return bad("unknown decision")
			}
		}
		var current, base, target *WorkflowState
		if wid != nil {
			current, err = workflowState(ctx, tx, account, *wid)
			if err != nil {
				return err
			}
		}
		if action == "undo" {
			if err = json.Unmarshal(after, &base); err != nil {
				return err
			}
			if err = json.Unmarshal(before, &target); err != nil {
				return err
			}
		} else {
			if err = json.Unmarshal(before, &base); err != nil {
				return err
			}
			if err = json.Unmarshal(proposed, &target); err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(current, base) {
			if action == "undo" {
				return conflict("workflow changed after approval; cannot undo")
			}
			refusal = conflict("proposal is stale; workflow changed")
			return set("stale")
		}
		if operation == "run" {
			run, err := e.StartRunTx(ctx, tx, account, *wid, input)
			if err != nil {
				return err
			}
			state, _ := json.Marshal(current)
			_, err = tx.Exec(ctx, "UPDATE workflow_proposals SET status='applied',after_state=$2,decided_at=$3,started_run_id=$4 WHERE id=$1", id, state, e.Now(), run)
			return err
		}
		if target == nil { // Undo a create: retain history and definition.
			copy := *current
			target = &copy
			target.Deleted = true
			target.IsDefault = false
		}
		workflowID := ""
		if wid != nil {
			workflowID = *wid
		}
		workflowID, target, err = e.applyWorkflowState(ctx, tx, account, workflowID, current, target)
		if err != nil {
			return err
		}
		wid = &workflowID
		state, _ := json.Marshal(target)
		if action == "undo" {
			_, err = tx.Exec(ctx, "UPDATE workflow_proposals SET status='undone',undo_state=$2,undone_at=$3 WHERE id=$1", id, state, e.Now())
		} else {
			_, err = tx.Exec(ctx, "UPDATE workflow_proposals SET status='applied',workflow_id=$2,after_state=$3,decided_at=$4 WHERE id=$1", id, *wid, state, e.Now())
		}
		return err
	})
	if err != nil {
		return err
	}
	return refusal
}
