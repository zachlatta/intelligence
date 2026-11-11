package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func qlimit(query string, limit int) string {
	b, _ := json.Marshal(map[string]any{"query": query, "limit": limit})
	return string(b)
}

func TestMCPOnlySQLReadSurface(t *testing.T) {
	want := map[string]bool{"readme": true, "schema_overview": true, "describe_table": true, "query": true,
		"propose_mutation": true, "withdraw_mutation": true, "propose_mutation_help": true, "comment_proposal": true}
	for _, tool := range mcpTools() {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %s", tool.Name)
		}
		readOnly := tool.Name != "propose_mutation" && tool.Name != "withdraw_mutation" && tool.Name != "comment_proposal"
		if tool.Annotations["readOnlyHint"] != readOnly {
			t.Errorf("wrong read-only annotation for %s", tool.Name)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
	for _, name := range []string{"schema", "list_agents", "read_transcript", "list_runs", "read_run"} {
		if _, _, _, err := mcpQuery(name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("removed tool %s still callable", name)
		}
	}
}

func TestMCPReadmeGuidesSQL(t *testing.T) {
	h, _ := dbServer(t)
	_, token := mintMCP(t, h)
	got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readme","arguments":{}}}`)
	for _, term := range []string{"schema_overview", "describe_table", "workflow_runs", "workflow_run_events", "recorded_agents", "transcripts", "seq_start", "epoch", "synced_at", "LIMIT", "SQL"} {
		if !strings.Contains(got, term) {
			t.Errorf("readme missing %s: %s", term, got)
		}
	}
}

func TestMCPProposalGuidance(t *testing.T) {
	for _, term := range []string{"propose_mutation", "withdraw_mutation", "propose_mutation_help", "browser", "workflow_proposals"} {
		if !strings.Contains(mcpReadme, term) {
			t.Errorf("guide missing %s", term)
		}
	}
	if !strings.Contains(mcpInstructions, "propose_mutation") || !strings.Contains(mcpInstructions, "browser") {
		t.Fatal("initialization omits reviewed proposals")
	}
	if strings.Contains(mcpReadme, "MCP credentials cannot write") {
		t.Fatal("guide denies supported proposal writes")
	}
}

// Keep the shipped guide executable, and verify its reads use the same SQL API path.
func TestMCPGuideExamples(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "Guide fixture")
	sendBatch(t, h, batch(true, "guide-epoch", tentry(0, `{"type":"user_message","text":"guide message"}`)))
	_, token := mintMCP(t, h)
	parts := strings.Split(mcpReadme, "```sql\n")
	if len(parts) != 7 {
		t.Fatalf("expected six SQL examples, got %d", len(parts)-1)
	}
	for _, part := range parts[1:] {
		query := strings.SplitN(part, "```", 2)[0]
		query = strings.NewReplacer("example-host", "host-1", "example-agent", agentA1,
			"00000000-0000-4000-8000-000000000001", run, "lower('task')", "lower('')").Replace(query)
		code, api := sqlPost(t, h, q(query))
		if code != 200 || api.RowCount == 0 {
			t.Fatalf("guide SQL: %s: %d %+v", query, code, api)
		}
		got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":`+q(query)+`}}`)
		var msg struct {
			Result struct {
				StructuredContent sqlResult `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(got), &msg); err != nil {
			t.Fatal(err)
		}
		mcpJSON, _ := json.Marshal(msg.Result.StructuredContent)
		apiJSON, _ := json.Marshal(api)
		if string(mcpJSON) != string(apiJSON) {
			t.Fatalf("guide SQL differs from API: %s: %s vs %s", query, mcpJSON, apiJSON)
		}
	}
}
