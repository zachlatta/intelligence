package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// Stateless Streamable HTTP MCP. Host keys grant account-scoped SQL reads and
// proposal creation/withdrawal, never workflow writes or human decisions.
func (a *app) workflowMCP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) || (r.Header.Get("Origin") != "" && strings.TrimRight(r.Header.Get("Origin"), "/") != a.baseURL(r)) {
		http.Error(w, "cross-site request refused", 403)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && version != "2025-03-26" && version != "2025-06-18" && version != "2025-11-25" {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return
	}
	v, by, ok := a.proposalAgent(w, r)
	if !ok {
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	respond := func(result any, code int, message string) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if code != 0 {
			out["error"] = map[string]any{"code": code, "message": message}
		} else {
			out["result"] = result
		}
		writeJSON(w, out)
	}
	if req.JSONRPC != "2.0" {
		respond(nil, -32600, "jsonrpc must be 2.0")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version != "2025-03-26" && version != "2025-06-18" && version != "2025-11-25" {
			version = "2025-03-26"
		}
		respond(map[string]any{"protocolVersion": version, "serverInfo": map[string]string{"name": "intelligence", "version": a.cfg.Version}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": "Read account state with query. Agents propose and withdraw only; share the approvalURL for browser review. Approval, rejection and undo require a signed-in human."}, 0, "")
	case "ping":
		respond(map[string]any{}, 0, "")
	case "tools/list":
		respond(map[string]any{"tools": proposalTools}, 0, "")
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			respond(nil, -32602, err.Error())
			return
		}
		var value any
		var err error
		switch params.Name {
		case "propose_mutation_help":
			example := workflow.Starting
			example.Host = strings.TrimPrefix(by, "host:")
			value = map[string]any{"description": proposalHelp, "schema": proposalSchema, "example": map[string]any{"operation": "create", "summary": "Add a task workflow", "workflow": map[string]any{"name": "Task workflow", "description": "", "definition": example}}}
		case "propose_mutation":
			var input workflow.ProposalRequest
			err = strictArguments(params.Arguments, &input)
			if err == nil {
				var id string
				id, err = a.wf.Propose(r.Context(), v.acct.ID, by, input)
				if err == nil {
					value = map[string]string{"id": id, "approvalURL": a.proposalURL(r, id)}
				}
			}
		case "withdraw_mutation":
			var input struct {
				ID     string `json:"id"`
				Reason string `json:"reason"`
			}
			err = strictArguments(params.Arguments, &input)
			if err == nil {
				err = a.wf.Decide(r.Context(), v.acct.ID, by, input.ID, "withdraw", input.Reason)
				value = map[string]string{"id": input.ID, "status": "withdrawn"}
			}
		case "query":
			var input sqlRequest
			err = strictArguments(params.Arguments, &input)
			if err == nil {
				if input.Query == "" {
					err = fmt.Errorf("query is required")
				} else {
					if input.Limit <= 0 {
						input.Limit = defaultSQLLimit
					}
					input.Limit = min(input.Limit, maxSQLLimit)
					value, err = runReadonly(r.Context(), a.cfg, v.pool, input)
				}
			}
		default:
			respond(nil, -32602, "unknown tool")
			return
		}
		isError := err != nil
		var b []byte
		if isError {
			b = []byte(err.Error())
		} else {
			b, _ = json.Marshal(value)
		}
		respond(map[string]any{"content": []any{map[string]string{"type": "text", "text": string(b)}}, "isError": isError}, 0, "")
	default:
		respond(nil, -32601, "method not found")
	}
}

func strictArguments(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

const proposalHelp = `One proposal changes one workflow atomically. operation=create requires workflow {name, description, definition}; operation=update requires workflowId, baseVersion and a complete replacement workflow; operation=delete requires workflowId and baseVersion, with no workflow. summary explains why. Read workflows and workflow_versions using query first; preserve fields you do not intend to change. definition uses the existing workflow format (host, trigger, project, orchestrator, steps, optional email). Steps contain id, kind, title and instructions; orchestrator contains provider, model and thinking. The example includes a valid complete definition. No change runs until the account owner opens approvalURL and approves. Proposals expire after seven days. Review/undo conflicts if current state differs from the stored snapshot. Read/list proposals with query against workflow_proposals (before_state, proposed_state, after_state, status, expires_at, undo_state, undone_at). withdraw_mutation requires id and reason, and takes back a pending proposal from this same host. propose_mutation accepts optional proposedBy (Paseo agent identity), runId (workflow run identity), and replacesId; replacement atomically withdraws your old pending, unexpired proposal with a recorded reason. Setting the default workflow is not proposable; updates preserve the default while the trigger remains task. There is no agent approval or undo tool.`

var proposalSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operation","summary"],"properties":{"operation":{"type":"string","enum":["create","update","delete"]},"summary":{"type":"string","minLength":1,"maxLength":2000},"workflowId":{"type":"string","description":"Workflow UUID, required for update/delete"},"baseVersion":{"type":"integer","minimum":1,"description":"Current workflows.version, required for update/delete"},"workflow":{"type":"object","additionalProperties":false,"required":["name","definition"],"properties":{"name":{"type":"string","maxLength":100},"description":{"type":"string"},"definition":{"type":"object","description":"Full workflow definition; call propose_mutation_help for a valid example, or read workflow_versions.definition with query"}}},"proposedBy":{"type":"string","maxLength":500,"description":"Optional Paseo agent id or proposer label; host authentication remains authoritative"},"runId":{"type":"string","maxLength":200,"description":"Optional workflow run id for attribution"},"replacesId":{"type":"string","description":"Your pending proposal UUID to withdraw atomically when this proposal is created"}}}`)

var proposalTools = []any{
	map[string]any{"name": "propose_mutation", "description": "Propose creating, replacing or deleting a workflow. Does not apply it. Returns id and approvalURL for signed-in human review. Call propose_mutation_help first.", "inputSchema": proposalSchema},
	map[string]any{"name": "withdraw_mutation", "description": "Withdraw a pending proposal made by this host. Refuses already applied or decided proposals.", "inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"required":["id","reason"],"properties":{"id":{"type":"string"},"reason":{"type":"string","minLength":1,"maxLength":2000}}}`)},
	map[string]any{"name": "propose_mutation_help", "description": "Mutation format, full workflow example, conflict/undo rules and SQL discovery guidance.", "inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false}`)},
	map[string]any{"name": "query", "description": "Read account state using bounded read-only SQL. Discover columns in information_schema.columns; read workflows, workflow_versions and workflow_proposals. The database role cannot write.", "inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":10000}}}`), "annotations": map[string]bool{"readOnlyHint": true}},
}
