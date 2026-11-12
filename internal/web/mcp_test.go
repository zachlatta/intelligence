package web

import (
	"encoding/json"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mintMCP(t *testing.T, h http.Handler) (string, string) {
	t.Helper()
	r := call(t, h, "POST", "/api/mcp-tokens", `{}`, sameOriginFetch)
	var tok struct{ ID, Token string }
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &tok) != nil || tok.Token == "" {
		t.Fatalf("mint: %d %s", r.Code, r.Body)
	}
	return tok.ID, tok.Token
}

func TestMCPProposalTokenIdentityAndApprovalBoundary(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	tokenID, token := mintMCP(t, h)
	created := time.Date(2026, time.November, 1, 12, 30, 0, 0, time.UTC)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE mcp_tokens SET created_at=$2 WHERE id=$1", tokenID, created); err != nil {
		t.Fatal(err)
	}
	_, otherToken := mintMCP(t, h)
	def := workflow.Starting
	def.Host = "host-1"
	arguments, _ := json.Marshal(map[string]any{"operation": "create", "summary": "Token proposal", "workflow": map[string]any{"name": "Token workflow", "definition": def}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_mutation","arguments":` + string(arguments) + `}}`
	got := rpc(t, h, token, body)
	var out struct {
		Result struct {
			StructuredContent struct{ ID, ApprovalURL string }
		}
	}
	json.Unmarshal([]byte(got), &out)
	id := out.Result.StructuredContent.ID
	if id == "" || out.Result.StructuredContent.ApprovalURL == "" {
		t.Fatalf("proposal: %s", got)
	}
	var proposer string
	if err := d.Owner.QueryRow(t.Context(), "SELECT proposer FROM workflow_proposals WHERE id=$1", id).Scan(&proposer); err != nil || proposer != "mcp:"+tokenID {
		t.Fatalf("identity: %q %v", proposer, err)
	}
	for _, path := range []string{"/workflow-proposals/" + id, "/workflow-proposals"} {
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, "mcp:"+tokenID) || !strings.Contains(page, `MCP token created <time datetime="2026-11-01T`) {
			t.Errorf("%s lacks readable token attribution: %s", path, page)
		}
	}
	for _, action := range []string{"approve", "reject", "undo"} {
		r := call(t, anon(h), "POST", "/api/workflow-proposals/"+id+"/"+action, `{}`, map[string]string{"Authorization": "Bearer " + token})
		if r.Code != 401 {
			t.Fatalf("token can %s: %d", action, r.Code)
		}
	}
	withdraw := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"withdraw_mutation","arguments":{"id":"` + id + `","reason":"Changed plan"}}}`
	if other := rpc(t, h, otherToken, withdraw); !strings.Contains(other, `"isError":true`) {
		t.Fatal("other token withdrew proposal: " + other)
	}
	if own := rpc(t, h, token, withdraw); strings.Contains(own, `"isError":true`) {
		t.Fatal(own)
	}
	r := call(t, h, "DELETE", "/api/mcp-tokens/"+tokenID, "", sameOriginFetch)
	if r.Code != 204 {
		t.Fatal(r.Body)
	}
	if page := get(t, h, "/workflow-proposals/"+id, nil); page.Code != 200 || !strings.Contains(page.Body.String(), "mcp:"+tokenID) {
		t.Fatal("revocation lost proposal attribution")
	}
	r = call(t, anon(h), "POST", "/mcp", body, map[string]string{"Authorization": "Bearer " + token})
	if r.Code != 401 {
		t.Fatal("revoked token proposed")
	}
}

func TestMCPProtocolErrorsAndOrigin(t *testing.T) {
	h, _ := dbServer(t)
	_, token := mintMCP(t, h)
	headers := map[string]string{"Authorization": "Bearer " + token}
	for _, tc := range []struct{ body, code string }{
		{`{`, `-32700`}, {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, `-32600`},
		{`{"jsonrpc":"2.0","id":1,"method":"missing"}`, `-32601`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"query":""}}}`, `-32602`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"describe_table","arguments":{"Table":"ignored"}}}`, `-32602`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"query":"SELECT 1","limit":10001}}}`, `-32602`},
	} {
		if got := rpc(t, h, token, tc.body); !strings.Contains(got, `"code":`+tc.code) {
			t.Errorf("%s: %s", tc.body, got)
		}
	}
	for _, method := range []string{"GET", "DELETE"} {
		if r := call(t, anon(h), method, "/mcp", "", headers); r.Code != 405 {
			t.Errorf("%s: %d", method, r.Code)
		}
	}
	for _, origin := range []string{"https://evil.example", "null", "https://example.com"} {
		r := call(t, anon(h), "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Authorization": "Bearer " + token, "Origin": origin, "Sec-Fetch-Site": "none"})
		if r.Code != 403 {
			t.Errorf("origin %s: %d", origin, r.Code)
		}
	}
	if r := call(t, h, "POST", "/api/mcp-tokens", `{}`, map[string]string{"Origin": "https://evil.example"}); r.Code != 403 {
		t.Fatal("cross-origin mint allowed")
	}
	if r := call(t, anon(h), "POST", "/api/mcp-tokens", `{}`, nil); r.Code != 401 {
		t.Fatal("anonymous mint allowed")
	}
}

func TestMCPPagingAndTimeout(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "test", func(c *Config) { c.SQLTimeout = 50 * time.Millisecond })
	sendReport(t, h, sampleReport)
	sendBatch(t, h, batch(true, "epoch-1", tentry(0, `{"type":"user_message","text":"first"}`), tentry(1, `{"type":"assistant_message","text":"second"}`)))
	_, token := mintMCP(t, h)
	tool := func(name, args string) string {
		return rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	}
	got := tool("query", qlimit("SELECT t.epoch,t.synced_at,e.* FROM transcripts t JOIN transcript_entries e USING(host,agent_id) WHERE host='host-1' AND agent_id='"+agentA1+"' ORDER BY seq_start", 1))
	if !strings.Contains(got, `"truncated":true`) || !strings.Contains(got, "first") || strings.Contains(got, "second") || !strings.Contains(got, "epoch-1") {
		t.Fatal(got)
	}
	got = tool("query", q("SELECT * FROM transcript_entries WHERE host='host-1' AND agent_id='"+agentA1+"' AND seq_start>0 ORDER BY seq_start"))
	if strings.Contains(got, "first") || !strings.Contains(got, "second") || !strings.Contains(got, `"truncated":false`) {
		t.Fatal(got)
	}
	got = tool("describe_table", `{"table":"' OR true --"}`)
	if !strings.Contains(got, `"rowCount":0`) {
		t.Fatal(got)
	}
	got = tool("query", `{"query":"SELECT pg_sleep(5)"}`)
	if !strings.Contains(got, `"isError":true`) || !strings.Contains(got, "timeout") {
		t.Fatal(got)
	}
	got = tool("query", `{"query":"SELECT generate_series(1,10)","limit":2}`)
	if !strings.Contains(got, `"rowCount":2`) || !strings.Contains(got, `"truncated":true`) {
		t.Fatal(got)
	}
	// Password changes revoke integration credentials as well as browser sessions.
	if err := h.store.ChangePassword(t.Context(), h.acct.ID, testPassword, "a new password"); err != nil {
		t.Fatal(err)
	}
	if r := call(t, anon(h), "POST", "/mcp", `{}`, map[string]string{"Authorization": "Bearer " + token}); r.Code != 401 {
		t.Fatal("password change kept MCP token")
	}
}

func rpc(t *testing.T, h http.Handler, token, body string) string {
	t.Helper()
	r := call(t, anon(h), "POST", "/mcp", body, map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-06-18"})
	if r.Code != 200 {
		t.Fatalf("rpc: %d %s", r.Code, r.Body)
	}
	return r.Body.String()
}

func TestMCPTransportAndTools(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	_, token := mintMCP(t, h)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	} {
		if got := rpc(t, h, token, body); strings.Contains(got, `"error"`) {
			t.Fatal(got)
		}
	}
	for name, args := range map[string]string{
		"query": `{"query":"SELECT name FROM hosts"}`, "schema_overview": `{}`, "describe_table": `{"table":"agents"}`,
	} {
		got := rpc(t, h, token, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		if strings.Contains(got, `"isError":true`) || !strings.Contains(got, `"structuredContent"`) {
			t.Errorf("%s: %s", name, got)
		}
	}
	for _, query := range []string{"DELETE FROM hosts", "CREATE TABLE sneaky (id int)", "SELECT 1; DELETE FROM hosts", "SET ROLE postgres"} {
		args, _ := json.Marshal(map[string]string{"query": query})
		got := rpc(t, h, token, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"query","arguments":`+string(args)+`}}`)
		if !strings.Contains(got, `"isError":true`) {
			t.Errorf("write allowed: %s", got)
		}
	}
	r := call(t, anon(h), "POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{"Authorization": "Bearer " + token})
	if r.Code != 202 || r.Body.Len() != 0 {
		t.Fatalf("notification: %d %s", r.Code, r.Body)
	}
}

func TestMCPRunReadsAndAccountIsolation(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "MCP run fixture")
	_, token := mintMCP(t, h)
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "outsider", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	c := sessionFor(t, ts.store, other)
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.AddCookie(c); anon(h).ServeHTTP(w, r) })
	_, otherToken := mintMCP(t, otherH)
	sendBatch(t, h, batch(true, "epoch", tentry(1, `{"type":"user_message","text":"private message"}`)))
	for _, query := range []string{
		"SELECT * FROM recorded_agents",
		"SELECT * FROM workflow_runs",
		"SELECT * FROM workflow_run_events WHERE run_id='" + run + "'",
		"SELECT * FROM transcript_entries WHERE host='host-1' AND agent_id='" + agentA1 + "'",
	} {
		name := query
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":` + q(query) + `}}`
		got := rpc(t, h, token, body)
		if strings.Contains(got, `"rowCount":0`) || strings.Contains(got, `"isError":true`) {
			t.Errorf("own %s: %s", name, got)
		}
		got = rpc(t, h, otherToken, body)
		if !strings.Contains(got, `"rowCount":0`) || strings.Contains(got, "private message") || strings.Contains(got, run) {
			t.Errorf("other %s: %s", name, got)
		}
	}
}

func TestMCPSQLRunSummaryAndExplicitInput(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	input := "Summary title.\n\n" + strings.Repeat("long-task-payload ", 100)
	run := startRun(t, h, wf, input)
	_, token := mintMCP(t, h)
	got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":`+q("SELECT id,workflow_id,title,status,step,orchestrator_id,note,created_at,finished_at FROM workflow_runs ORDER BY created_at DESC,id DESC LIMIT 50")+`}}`)
	var msg struct {
		Result struct {
			StructuredContent sqlResponse `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &msg); err != nil {
		t.Fatal(err)
	}
	res := msg.Result.StructuredContent
	if res.RowCount != 1 || !strings.Contains(got, run) {
		t.Fatalf("missing run summary: %s", got)
	}
	columns := map[string]bool{}
	for _, col := range res.Columns {
		columns[col.Name] = true
	}
	for _, name := range []string{"id", "workflow_id", "title", "status", "step", "orchestrator_id", "note", "created_at", "finished_at"} {
		if !columns[name] {
			t.Errorf("missing summary column %s", name)
		}
	}
	if columns["input"] || strings.Contains(got, "long-task-payload") {
		t.Fatal("run listing includes task input")
	}
	args, _ := json.Marshal(map[string]string{"query": "SELECT input FROM workflow_runs WHERE id='" + run + "'"})
	got = rpc(t, h, token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":`+string(args)+`}}`)
	if !strings.Contains(got, "long-task-payload") {
		t.Fatalf("explicit input query failed: %s", got)
	}
}

func TestMCPConfiguredOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://backend.example/mcp", nil)
	r.Header.Set("Origin", "https://public.example")
	if !mcpOrigin(Config{PublicURL: "https://public.example"}, r) {
		t.Fatal("configured public origin refused")
	}
	r.Header.Set("Origin", "http://public.example")
	if mcpOrigin(Config{PublicURL: "https://public.example"}, r) {
		t.Fatal("wrong scheme allowed")
	}
}

func TestMCPVersionsAndTrailingSlash(t *testing.T) {
	h, _ := dbServer(t)
	_, token := mintMCP(t, h)
	for _, path := range []string{"/mcp", "/mcp/"} {
		for _, version := range []string{"2025-03-26", "2025-06-18", "2099-01-01"} {
			r := call(t, anon(h), "POST", path, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, map[string]string{"Authorization": "Bearer " + token})
			want := version
			if version == "2099-01-01" {
				want = "2025-06-18"
			}
			if r.Code != 200 || !strings.Contains(r.Body.String(), `"protocolVersion":"`+want+`"`) {
				t.Errorf("%s %s: %d %s", path, version, r.Code, r.Body)
			}
		}
		for _, version := range []string{"", "2025-03-26", "2025-06-18", "invalid"} {
			headers := map[string]string{"Authorization": "Bearer " + token, "MCP-Protocol-Version": version}
			r := call(t, anon(h), "POST", path, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":{"query":"SELECT 1"}}}`, headers)
			want := 200
			if version == "invalid" {
				want = 400
			}
			if r.Code != want {
				t.Errorf("%s header %s: %d %s", path, version, r.Code, r.Body)
			}
		}
		if r := call(t, anon(h), "POST", path, `{}`, nil); r.Code != 401 {
			t.Errorf("%s anonymous: %d", path, r.Code)
		}
	}
}

func TestAccountMCPTokenControls(t *testing.T) {
	h, _ := dbServer(t)
	id, secret := mintMCP(t, h)
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "tokenowner", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	c := sessionFor(t, ts.store, other)
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.AddCookie(c); anon(h).ServeHTTP(w, r) })
	otherID, _ := mintMCP(t, otherH)
	page := get(t, h, "/account", nil)
	if page.Code != 200 {
		t.Fatalf("account: %d %s", page.Code, page.Body)
	}
	for _, want := range []string{"Create MCP token", `data-token-id="` + id + `"`, "Revoke", "Shown once", "/api/mcp-tokens", `id="mcp-token-value"`, `id="mcp-url"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("account missing %s", want)
		}
	}
	if strings.Contains(page.Body.String(), secret) || strings.Contains(page.Body.String(), otherID) {
		t.Fatal("account exposed a token secret or another account's ID")
	}
	otherPage := call(t, otherH, "GET", "/account", "", nil)
	if strings.Contains(otherPage.Body.String(), id) || !strings.Contains(otherPage.Body.String(), otherID) {
		t.Fatal("token list is not account scoped")
	}
	if r := call(t, h, "DELETE", "/api/mcp-tokens/"+id, "", nil); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if strings.Contains(get(t, h, "/account", nil).Body.String(), id) {
		t.Fatal("revoked token still listed")
	}
}

func TestMCPTokenIsolationAndRevocation(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, sampleReport)
	id, token := mintMCP(t, h)
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	c := sessionFor(t, ts.store, other)
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.AddCookie(c); anon(h).ServeHTTP(w, r) })
	_, otherToken := mintMCP(t, otherH)
	got := rpc(t, h, otherToken, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"query":"SELECT name FROM hosts"}}}`)
	if strings.Contains(got, "host-1") || !strings.Contains(got, `"rowCount":0`) {
		t.Fatal(got)
	}
	for _, tok := range []string{"", "bad", ts.cookie.Value, keyOf(t, h, "host-1")} {
		if r := call(t, anon(h), "POST", "/mcp", `{}`, map[string]string{"Authorization": "Bearer " + tok}); r.Code != 401 {
			t.Errorf("invalid token: %d", r.Code)
		}
	}
	if r := call(t, h, "POST", "/mcp", `{}`, nil); r.Code != 401 {
		t.Errorf("cookie authorized MCP: %d", r.Code)
	}
	if r := call(t, anon(h), "POST", "/api/workflows", `{}`, map[string]string{"Authorization": "Bearer " + token}); r.Code != 401 {
		t.Errorf("token can write: %d", r.Code)
	}
	if r := call(t, otherH, "DELETE", "/api/mcp-tokens/"+id, "", nil); r.Code != 404 {
		t.Fatalf("other revoked token: %d", r.Code)
	}
	if r := call(t, h, "DELETE", "/api/mcp-tokens/"+id, "", nil); r.Code != 204 {
		t.Fatalf("revoke: %d %s", r.Code, r.Body)
	}
	if r := call(t, anon(h), "POST", "/mcp", `{}`, map[string]string{"Authorization": "Bearer " + token}); r.Code != 401 {
		t.Fatal("revoked token works")
	}
	id, token = mintMCP(t, h)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE mcp_tokens SET expires_at = now() - interval '1 second' WHERE id::text=$1", id); err != nil {
		t.Fatal(err)
	}
	if r := call(t, anon(h), "POST", "/mcp", `{}`, map[string]string{"Authorization": "Bearer " + token}); r.Code != 401 {
		t.Fatal("expired token works")
	}
}

func TestMCPWorkflowProposals(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	_, key := mintMCP(t, h)
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
