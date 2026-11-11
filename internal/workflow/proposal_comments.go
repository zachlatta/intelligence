package workflow

import (
	"context"
	"strings"
)

// Comment records discussion without approving, changing, or resuming a workflow.
// Author is supplied by the authenticated transport, never by the request body.
func (e *Engine) Comment(ctx context.Context, account int64, proposal, author, text string) error {
	if !uuidRe.MatchString(proposal) {
		return ErrNotFound
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 20000 {
		return bad("comment must contain 1 to 20000 bytes")
	}
	tag, err := e.p.Exec(ctx, `INSERT INTO workflow_proposal_comments (proposal_id,author,text,created_at)
 SELECT id,$3,$4,$5 FROM workflow_proposals WHERE id=$1 AND account_id=$2`, proposal, account, author, text, e.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
