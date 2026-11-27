package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// A model-only variant is a workflow that follows another one, its source (workflows.variant_of):
// it runs the source's current host, priority, project, orchestrator instructions and steps, and
// keeps only its own orchestrator agent (provider, model, thinking, speed) and its own trigger
// (trigger, API trigger, email or voice memo filter), so it can have its own email address. Each
// save of the source saves its variants again, in the same transaction, so a change to the source
// reaches every variant as a new version of it. A variant cannot itself have variants.

// AsVariantOf is v's definition as a variant of source: source's, with v's own orchestrator agent
// and trigger. It shares nothing with source.
func (v Definition) AsVariantOf(source Definition) Definition {
	var d Definition
	b, _ := json.Marshal(source)
	_ = json.Unmarshal(b, &d)
	d.Trigger, d.APITrigger, d.Email, d.VoiceMemo = v.Trigger, v.APITrigger, nil, nil
	if v.Email != nil {
		e := *v.Email
		e.From = append([]string(nil), v.Email.From...)
		d.Email = &e
	}
	if v.VoiceMemo != nil {
		m := *v.VoiceMemo
		d.VoiceMemo = &m
	}
	d.Orchestrator.Provider = v.Orchestrator.Provider
	d.Orchestrator.Model = v.Orchestrator.Model
	d.Orchestrator.Thinking = v.Orchestrator.Thinking
	d.Orchestrator.ServiceTier = v.Orchestrator.ServiceTier
	return d
}

// InheritsFrom says whether v already has everything a variant takes from source.
func (v Definition) InheritsFrom(source Definition) bool {
	return sameDefinition(v, v.AsVariantOf(source))
}

func sameDefinition(a, b Definition) bool {
	x, _ := json.Marshal(a.normalized())
	y, _ := json.Marshal(b.normalized())
	return bytes.Equal(x, y)
}

// variantSource is the definition the workflow id follows and the source's name, or nil when id
// is no variant. The caller holds the account lock; the source's row is locked too.
func variantSource(ctx context.Context, tx pgx.Tx, account int64, id string) (*WorkflowState, string, error) {
	if id == "" {
		return nil, "", nil
	}
	var source *string
	err := tx.QueryRow(ctx, `SELECT variant_of::text FROM workflows WHERE id=$1 AND account_id=$2`, id, account).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && source == nil) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	s, err := workflowState(ctx, tx, account, *source)
	if err != nil {
		return nil, "", err
	}
	return s, *source, nil
}

// setVariantOf makes id a variant of source, or with source "", no variant. The caller holds the
// account lock, and saves id next, which composes it from its source.
func setVariantOf(ctx context.Context, tx pgx.Tx, account int64, id, source string) error {
	if source == "" {
		_, err := tx.Exec(ctx, `UPDATE workflows SET variant_of=NULL WHERE id=$1 AND account_id=$2`, id, account)
		return err
	}
	if err := checkVariantSource(ctx, tx, account, id, source); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE workflows SET variant_of=$3 WHERE id=$1 AND account_id=$2`, id, account, source)
	return err
}

// checkVariantSource refuses a source that is not one of account's live workflows, is id itself,
// or is a variant; and refuses to make id (when it is saved already) a variant while it has its own.
func checkVariantSource(ctx context.Context, tx pgx.Tx, account int64, id, source string) error {
	if !uuidRe.MatchString(source) {
		return ErrNotFound
	}
	if source == id {
		return bad("a workflow cannot be a variant of itself")
	}
	s, err := workflowState(ctx, tx, account, source)
	if err != nil {
		return err
	}
	if s.Deleted {
		return ErrNotFound
	}
	var sourceIsVariant, hasVariants bool
	if err := tx.QueryRow(ctx, `SELECT variant_of IS NOT NULL FROM workflows WHERE id=$1`, source).Scan(&sourceIsVariant); err != nil {
		return err
	}
	if sourceIsVariant {
		return bad("“%s” is itself a variant: make this one a variant of the workflow it follows", s.Name)
	}
	if id != "" {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows WHERE variant_of::text=$1 AND deleted_at IS NULL)`, id).Scan(&hasVariants); err != nil {
			return err
		}
		if hasVariants {
			return bad("this workflow has variants of its own, so it cannot be a variant")
		}
	}
	return nil
}

// saveVariants saves source's live variants again from its definition d, each as a new version
// when it changes. The caller holds the account lock and has just saved source.
func (e *Engine) saveVariants(ctx context.Context, tx pgx.Tx, account int64, source string, d Definition) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM workflows WHERE variant_of=$1 AND account_id=$2 AND deleted_at IS NULL ORDER BY created_at, id`, source, account)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		current, err := workflowState(ctx, tx, account, id)
		if err != nil {
			return err
		}
		if current.Definition.InheritsFrom(d) {
			continue
		}
		target := *current
		target.Definition = current.Definition.AsVariantOf(d)
		if _, _, err := e.applyWorkflowState(ctx, tx, account, id, current, &target); err != nil {
			var ref *Refusal
			if errors.As(err, &ref) {
				return &Refusal{Conflict: ref.Conflict, Forbidden: ref.Forbidden, Msg: "its variant “" + current.Name + "” cannot take this change: " + ref.Msg}
			}
			return err
		}
	}
	return nil
}

// keepsInherited refuses d for the workflow id when id is a variant and d changes what it takes
// from its source: those changes belong on the source, where they reach every variant.
func keepsInherited(ctx context.Context, tx pgx.Tx, account int64, id string, d Definition) error {
	source, _, err := variantSource(ctx, tx, account, id)
	if err != nil || source == nil || d.InheritsFrom(source.Definition) {
		return err
	}
	return bad("this workflow is a model variant of “%s”: its host, priority, project, orchestrator instructions and steps come from there. "+
		"Change them on “%s”, or detach this variant first (variantOf \"\")", source.Name, source.Name)
}
