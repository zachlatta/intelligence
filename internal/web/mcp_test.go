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

func TestMCPProtocolErrorsAndOrigin(t *testing.T) {
	h, _ := dbServer(t)
	_, token := mintMCP(t, h)
	headers := map[string]string{"Authorization": "Bearer " + token}
	for _, tc := range []struct{ body, code string }{
		{`{`, `-32700`}, {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, `-32600`},
		{`{"jsonrpc":"2.0","id":1,"method":"missing"}`, `-32601`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"query":""}}}`, `-32602`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_agents","arguments":{"Host":"ignored"}}}`, `-32602`},
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
	got := tool("read_transcript", `{"host":"host-1","agentId":"`+agentA1+`","limit":1}`)
	if !strings.Contains(got, `"truncated":true`) || !strings.Contains(got, "first") || strings.Contains(got, "second") || !strings.Contains(got, "epoch-1") {
		t.Fatal(got)
	}
	got = tool("read_transcript", `{"host":"host-1","agentId":"`+agentA1+`","afterSeq":0}`)
	if strings.Contains(got, "first") || !strings.Contains(got, "second") || !strings.Contains(got, `"truncated":false`) {
		t.Fatal(got)
	}
	got = tool("list_agents", `{"host":"' OR true --"}`)
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
		"query": `{"query":"SELECT name FROM hosts"}`, "schema": `{}`, "describe_table": `{"table":"agents"}`,
		"list_agents": `{}`, "read_transcript": `{"host":"host-1","agentId":"` + agentA1 + `"}`, "list_runs": `{}`,
		"read_run": `{"runId":"00000000-0000-4000-8000-000000000001"}`,
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
	for name, args := range map[string]string{
		"list_agents": `{}`, "list_runs": `{}`, "read_run": `{"runId":"` + run + `"}`, "read_transcript": `{"host":"host-1","agentId":"` + agentA1 + `"}`,
	} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
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
