package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowMCP(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	headers := map[string]string{"Authorization": "Bearer " + key}
	rpc := func(method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		r := call(t, anon(h), "POST", "/mcp", string(b), headers)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", method, r.Code, r.Body)
		}
		var result map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	r := rpc("initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if r["result"] == nil {
		t.Fatal(r)
	}
	r = rpc("tools/list", nil)
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), "propose_mutation") || strings.Contains(string(b), `"name":"approve`) {
		t.Fatal(string(b))
	}
	d := workflow.Starting
	d.Host = "host-1"
	r = rpc("tools/call", map[string]any{"name": "propose_mutation", "arguments": map[string]any{"operation": "create", "summary": "Add workflow", "workflow": map[string]any{"name": "Proposed", "definition": d}}})
	b, _ = json.Marshal(r)
	if !strings.Contains(string(b), "approvalURL") {
		t.Fatal(string(b))
	}
	r = rpc("tools/call", map[string]any{"name": "query", "arguments": map[string]any{"query": "SELECT status FROM workflow_proposals"}})
	b, _ = json.Marshal(r)
	if !strings.Contains(string(b), "pending") {
		t.Fatal(string(b))
	}
	r = rpc("tools/call", map[string]any{"name": "approve_mutation", "arguments": map[string]any{}})
	if r["error"] == nil {
		t.Fatal("approval tool accepted")
	}
	bad := call(t, anon(h), "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Authorization": "Bearer " + key, "MCP-Protocol-Version": "unsupported"})
	if bad.Code != 400 {
		t.Fatalf("invalid protocol accepted: %d", bad.Code)
	}
}
