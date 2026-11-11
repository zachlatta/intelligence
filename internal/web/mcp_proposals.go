package web

import (
	"bytes"
	"encoding/json"
	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/workflow"
	"net/http"
)

func proposalMCPTools() []mcpTool {
	var schema map[string]any
	_ = json.Unmarshal(proposalSchema, &schema)
	return []mcpTool{
		{"comment_proposal", "Post an answer or comment in a proposal discussion. Include questionId to complete a dispatched answer job idempotently. Read workflow_proposal_comments and workflow_proposal_question_status with query first. Does not approve or change workflows.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "text"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "text": map[string]any{"type": "string", "minLength": 1, "maxLength": 20000}, "questionId": map[string]any{"type": "integer", "minimum": 1, "description": "Optional dispatched reviewer question id; links one answer and completes its job"}}}, map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}},
		{"propose_mutation", "Propose creating, replacing or deleting a workflow, or starting a workflow run after approval. No workflow edits or runs until a browser-session review approves it. Returns id and approvalURL. Call propose_mutation_help first.", schema, map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}},
		{"withdraw_mutation", "Withdraw this MCP token's pending proposal with a required reason; cannot withdraw another token's or host's proposal.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "reason"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}}}, map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}},
		{"propose_mutation_help", "Workflow mutation schema, full example, review/undo rules and SQL discovery guidance.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}, map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}},
	}
}

func (a *app) mcpProposalCall(w http.ResponseWriter, r *http.Request, id json.RawMessage, acct accounts.Account, proposer, name string, args json.RawMessage) {
	parse := func(target any) error {
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		return decodeMCP(dec, target)
	}
	var result any
	var err error
	switch name {
	case "comment_proposal":
		var req struct {
			ID         string `json:"id"`
			Text       string `json:"text"`
			QuestionID int64  `json:"questionId,omitempty"`
		}
		if err := parse(&req); err != nil {
			mcpError(w, id, -32602, err.Error())
			return
		}
		switch {
		case req.QuestionID < 0:
			mcpError(w, id, -32602, "invalid questionId")
			return
		case req.QuestionID > 0:
			err = a.wf.AnswerProposal(r.Context(), acct.ID, req.ID, req.QuestionID, proposer, req.Text)
		default:
			err = a.wf.Comment(r.Context(), acct.ID, req.ID, proposer, req.Text)
		}
		result = map[string]string{"id": req.ID, "status": "posted"}
	case "propose_mutation_help":
		if err := parse(&struct{}{}); err != nil {
			mcpError(w, id, -32602, err.Error())
			return
		}
		example := workflow.Starting
		example.Host = "your-host"
		pool, poolErr := a.store.Reader(r.Context(), acct)
		if poolErr != nil {
			mcpError(w, id, -32603, "reader unavailable")
			return
		}
		rows, readErr := runReadonly(r.Context(), a.cfg, pool, sqlRequest{Query: "SELECT host FROM host_keys ORDER BY host LIMIT 1", Limit: 1})
		if readErr != nil {
			mcpError(w, id, -32603, "host lookup unavailable")
			return
		}
		if len(rows.Rows) > 0 {
			example.Host = rows.Rows[0][0].(string)
		}
		result = map[string]any{"description": proposalHelp, "schema": proposalSchema, "example": map[string]any{"operation": "create", "summary": "Add a task workflow", "workflow": map[string]any{"name": "Task workflow", "description": "", "definition": example}}}
	case "propose_mutation":
		var req workflow.ProposalRequest
		if err := parse(&req); err != nil {
			mcpError(w, id, -32602, err.Error())
			return
		}
		var proposalID string
		proposalID, err = a.wf.Propose(r.Context(), acct.ID, proposer, req)
		if err == nil {
			result = map[string]string{"id": proposalID, "approvalURL": a.proposalURL(r, proposalID)}
		}
	case "withdraw_mutation":
		var req struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := parse(&req); err != nil {
			mcpError(w, id, -32602, err.Error())
			return
		}
		err = a.wf.Withdraw(r.Context(), acct.ID, proposer, req.ID, req.Reason)
		result = map[string]string{"id": req.ID, "status": "withdrawn"}
	}
	if err != nil {
		mcpResult(w, id, map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}})
		return
	}
	body, _ := json.Marshal(result)
	mcpResult(w, id, map[string]any{"isError": false, "structuredContent": result, "content": []any{map[string]string{"type": "text", "text": string(body)}}})
}

const proposalHelp = `One proposal changes one workflow or starts one run atomically. operation=run requires workflowId and input (the full task); baseVersion is optional, and the current complete workflow is snapshotted when proposed. Do not send workflow for run proposals. It creates no run or launch command before browser approval. Approval refuses drift, starts exactly one run and records started_run_id; repeat approval cannot create another. Run proposals cannot be undone; cancel the resulting run separately using browser controls. input is only valid for run proposals. operation=create requires workflow {name, description, definition}; operation=update requires workflowId, baseVersion and a complete replacement workflow; operation=delete requires workflowId and baseVersion, with no workflow. summary is safe Markdown (up to 2000 bytes); explain what changes and why. Read workflow_proposal_comments ordered by id to see reviewer questions. Reviewer questions automatically dispatch answer agents. Answer with comment_proposal {id,questionId,text}; text is safe Markdown, up to 20000 bytes. The questionId links one answer idempotently to its dispatch job; omit it for a standalone comment. Agent comments do not dispatch more agents or approve changes. Read workflows and workflow_versions using query first; preserve fields you do not intend to change. definition uses the existing workflow format (host, trigger, project, orchestrator, steps, optional email). Steps contain id, kind, title and instructions; orchestrator contains provider, model and thinking. The example includes a valid complete definition. No change runs until the account owner opens approvalURL and approves. Proposals expire after seven days. Review/undo conflicts if current state differs from the stored snapshot. Read/list proposals with query against workflow_proposals (before_state, proposed_state, after_state, status, expires_at, undo_state, undone_at). withdraw_mutation requires id and reason, and takes back a pending proposal from this same MCP token. propose_mutation accepts optional proposedBy (Paseo agent identity), runId (workflow run identity), and replacesId; replacement atomically withdraws this MCP token's old pending, unexpired proposal with a recorded reason. Setting the default workflow is not proposable; updates preserve the default while the trigger remains task. There is no agent approval or undo tool.`

var proposalSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operation","summary"],"properties":{"operation":{"type":"string","enum":["create","update","delete","run"]},"summary":{"type":"string","minLength":1,"maxLength":2000},"workflowId":{"type":"string","description":"Workflow UUID, required for update/delete/run"},"baseVersion":{"type":"integer","minimum":1,"description":"Current workflows.version, required for update/delete; optional drift guard for run"},"input":{"type":"string","minLength":1,"maxLength":1048576,"description":"Full task input, required only for operation=run; quoted for review, never executed before approval"},"workflow":{"type":"object","additionalProperties":false,"required":["name","definition"],"properties":{"name":{"type":"string","maxLength":100},"description":{"type":"string"},"definition":{"type":"object","description":"Full workflow definition; call propose_mutation_help for a valid example, or read workflow_versions.definition with query"}}},"proposedBy":{"type":"string","maxLength":500,"description":"Optional Paseo agent id or proposer label; authenticated credential remains authoritative"},"runId":{"type":"string","maxLength":200,"description":"Optional workflow run id for attribution"},"replacesId":{"type":"string","description":"Your pending proposal UUID to withdraw atomically when this proposal is created"}}}`)
