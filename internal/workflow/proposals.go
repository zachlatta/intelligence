package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

// WorkflowState is the complete editable state captured for review and undo.
// Version is a monotonic revision, including deletions and restores.
type WorkflowState struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Definition  Definition `json:"definition"`
	Version     int        `json:"version"`
	IsDefault   bool       `json:"isDefault"`
	Deleted     bool       `json:"deleted"`
}

type ProposalRequest struct {
	Operation   string         `json:"operation"`
	WorkflowID  string         `json:"workflowId,omitempty"`
	BaseVersion int            `json:"baseVersion,omitempty"`
	Summary     string         `json:"summary"`
	Workflow    *WorkflowState `json:"workflow,omitempty"`
}

func workflowState(ctx context.Context, tx pgx.Tx, account int64, id string) (*WorkflowState, error) {
	var s WorkflowState
	var def []byte
	err := tx.QueryRow(ctx, `SELECT w.name,w.description,w.version,w.is_default,w.deleted_at IS NOT NULL,v.definition
 FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version
 WHERE w.id=$1 AND w.account_id=$2 FOR UPDATE OF w`, id, account).Scan(&s.Name, &s.Description, &s.Version, &s.IsDefault, &s.Deleted, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(def, &s.Definition)
	return &s, err
}

// Propose records a single atomic create/update/delete without changing a workflow.
func (e *Engine) Propose(ctx context.Context, account int64, proposer string, r ProposalRequest) (string, error) {
	if strings.TrimSpace(r.Summary) == "" || len(r.Summary) > 2000 {
		return "", bad("summary is required (up to 2000 characters)")
	}
	if r.Operation != "create" && r.Operation != "update" && r.Operation != "delete" {
		return "", bad("operation must be create, update or delete")
	}
	if r.Operation == "create" {
		if r.WorkflowID != "" || r.BaseVersion != 0 {
			return "", bad("create takes no workflowId or baseVersion")
		}
	} else if !uuidRe.MatchString(r.WorkflowID) || r.BaseVersion < 1 {
		return "", bad("workflowId and baseVersion are required")
	}
	if r.Operation == "delete" {
		if r.Workflow != nil {
			return "", bad("delete takes no workflow")
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
		if r.Workflow.Version != 0 || r.Workflow.IsDefault || r.Workflow.Deleted {
			return "", bad("workflow takes only name, description and definition")
		}
		if err := r.Workflow.Definition.Validate(); err != nil {
			return "", bad("%s", err)
		}
	}
	var id string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var before *WorkflowState
		if r.Operation != "create" {
			var err error
			before, err = workflowState(ctx, tx, account, r.WorkflowID)
			if err != nil {
				return err
			}
			if before.Deleted || before.Version != r.BaseVersion {
				return conflict("workflow changed; read its latest state and propose again")
			}
		}
		if r.Workflow != nil {
			if err := ownsHost(ctx, tx, account, r.Workflow.Definition.Host); err != nil {
				return err
			}
			if err := uniqueEmailTrigger(ctx, tx, account, r.WorkflowID, r.Workflow.Definition); err != nil {
				return err
			}
		}
		// Store the exact target, including automatic revision/default changes.
		if r.Operation == "delete" {
			copy := *before
			r.Workflow = &copy
			r.Workflow.Deleted, r.Workflow.IsDefault = true, false
		}
		r.Workflow.Version = 1
		if before != nil {
			r.Workflow.Version = before.Version + 1
			if r.Operation == "update" {
				r.Workflow.IsDefault = before.IsDefault && r.Workflow.Definition.Trigger == TriggerTask
			}
		}
		b, _ := json.Marshal(before)
		p, _ := json.Marshal(r.Workflow)
		return tx.QueryRow(ctx, `INSERT INTO workflow_proposals(account_id,proposer,operation,workflow_id,summary,before_state,proposed_state,created_at,expires_at)
   VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8::timestamptz,$8::timestamptz+interval '7 days') RETURNING id::text`, account, proposer, r.Operation, r.WorkflowID, r.Summary, b, p, e.Now()).Scan(&id)
	})
	return id, err
}

// Decide serializes on the proposal and workflow. The before/after comparison includes
// metadata and default state as well as the definition, so undo cannot overwrite later work.
func (e *Engine) Decide(ctx context.Context, account int64, proposer, id, action string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	var refusal error
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		// Account lock serializes proposal creates/restores and default restoration.
		if _, err := tx.Exec(ctx, "SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE", account); err != nil {
			return err
		}
		var status, by, op string
		var wid *string
		var before, proposed, after []byte
		var expired bool
		err := tx.QueryRow(ctx, `SELECT status,proposer,operation,workflow_id::text,before_state,proposed_state,after_state,expires_at <= $3
   FROM workflow_proposals WHERE id=$1 AND account_id=$2 FOR UPDATE`, id, account, e.Now()).Scan(&status, &by, &op, &wid, &before, &proposed, &after, &expired)
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
			if status != "applied" {
				return conflict("only an applied proposal can be undone")
			}
		} else {
			if status != "pending" {
				return conflict("proposal is %s", status)
			}
			if action == "withdraw" && by != proposer {
				return ErrNotFound
			}
			if expired {
				refusal = conflict("proposal expired; propose again")
				return set("expired")
			}
			if action == "withdraw" {
				return set("withdrawn")
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
		if target == nil { // Undo a create: retain history and definition.
			copy := *current
			target = &copy
			target.Deleted = true
			target.IsDefault = false
		}
		if target.Deleted {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_runs WHERE workflow_id=$1 AND finished_at IS NULL)`, *wid).Scan(&live); err != nil {
				return err
			}
			if live {
				return conflict("workflow has runs still going; finish or cancel them first")
			}
		} else {
			if err := ownsHost(ctx, tx, account, target.Definition.Host); err != nil {
				return err
			}
			workflowID := ""
			if wid != nil {
				workflowID = *wid
			}
			if err := uniqueEmailTrigger(ctx, tx, account, workflowID, target.Definition); err != nil {
				return err
			}
			if target.IsDefault {
				var other bool
				if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM workflows WHERE account_id=$1 AND is_default AND id::text<>$2)", account, workflowID).Scan(&other); err != nil {
					return err
				}
				if other {
					return conflict("another workflow is now the default; cannot restore the previous default")
				}
			}
		}
		target.Version = 1
		if current != nil {
			target.Version = current.Version + 1
		}
		if wid == nil {
			var created string
			err = tx.QueryRow(ctx, `INSERT INTO workflows(account_id,name,description,version,created_at,updated_at) VALUES($1,$2,$3,1,$4,$4) RETURNING id::text`, account, target.Name, target.Description, e.Now()).Scan(&created)
			if err != nil {
				return err
			}
			wid = &created
		} else {
			_, err = tx.Exec(ctx, `UPDATE workflows SET name=$2,description=$3,version=$4,is_default=$5,deleted_at=CASE WHEN $6 THEN $7::timestamptz ELSE NULL END,updated_at=$7 WHERE id=$1`, *wid, target.Name, target.Description, target.Version, target.IsDefault, target.Deleted, e.Now())
			if err != nil {
				return err
			}
		}
		def, _ := json.Marshal(target.Definition)
		if _, err = tx.Exec(ctx, "INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,$2,$3,$4,$5)", *wid, target.Version, account, def, e.Now()); err != nil {
			return err
		}
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
