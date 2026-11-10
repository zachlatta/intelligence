package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/zachlatta/intelligence/internal/accounts"
)

const mcpVersion = "2025-06-18"

//go:embed mcp_readme.md
var mcpReadme string

const mcpInstructions = "Intelligence data, scoped to the token's account. Call readme first for the SQL guide and examples. All state reads use query; use schema_overview to find relations and describe_table for exact columns. propose_mutation and withdraw_mutation only queue or withdraw workflow proposals; call propose_mutation_help for their schema. Applying changes requires authenticated browser approval; there is no approval, undo or direct workflow-write tool."

func supportedMCPVersion(v string) bool { return v == mcpVersion || v == "2025-03-26" }

func (a *app) createMCPToken(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if err := decodeMCP(json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)), &body); err != nil {
			sqlError(w, 400, "send {}")
			return
		}
		tok, err := a.store.CreateMCPToken(r.Context(), v.acct)
		if err != nil {
			sqlError(w, 500, "could not create MCP token")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tok)
	}
}

func (a *app) revokeMCPToken(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := a.store.RevokeMCPToken(r.Context(), v.acct, r.PathValue("id"))
		if err != nil {
			sqlError(w, 500, "could not revoke MCP token")
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// mcp is stateless Streamable HTTP: each authenticated request is independent. Only
// read tools use the account's pool; proposal tools can queue/withdraw changes only.
func (a *app) mcp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !mcpOrigin(a.cfg, r) {
		http.Error(w, "cross-site request refused", 403)
		return
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="intelligence-mcp"`)
		http.Error(w, "MCP bearer token required", 401)
		return
	}
	acct, tokenID, err := a.store.MCPIdentity(r.Context(), token)
	if errors.Is(err, accounts.ErrNoSession) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="intelligence-mcp", error="invalid_token"`)
		http.Error(w, "invalid or expired MCP token", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 500)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "only POST is supported (no SSE stream)", 405)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedMCPVersion(v) {
		http.Error(w, "unsupported MCP protocol version", 400)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", 415)
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSQLBody))
	if err := decodeMCP(dec, &msg); err != nil {
		mcpError(w, nil, -32700, "invalid JSON request")
		return
	}
	id := msg.ID
	if msg.JSONRPC != "2.0" || msg.Method == "" || (id != nil && !validMCPID(id)) {
		mcpError(w, nil, -32600, "invalid JSON-RPC request")
		return
	}
	if id == nil {
		if !strings.HasPrefix(msg.Method, "notifications/") {
			http.Error(w, "request id required", 400)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.ProtocolVersion == "" {
			mcpError(w, id, -32602, "protocolVersion required")
			return
		}
		version := p.ProtocolVersion
		if !supportedMCPVersion(version) {
			version = mcpVersion
		}
		mcpResult(w, id, map[string]any{"protocolVersion": version, "serverInfo": map[string]string{"name": "intelligence", "version": a.cfg.Version}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": mcpInstructions})
	case "ping":
		mcpResult(w, id, map[string]any{})
	case "tools/list":
		mcpResult(w, id, map[string]any{"tools": mcpTools()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.Name == "" {
			mcpError(w, id, -32602, "tool name required")
			return
		}
		if p.Name == "propose_mutation" || p.Name == "withdraw_mutation" || p.Name == "propose_mutation_help" {
			a.mcpProposalCall(w, r, id, acct, "mcp:"+tokenID, p.Name, p.Arguments)
			return
		}
		query, args, limit, err := mcpQuery(p.Name, p.Arguments)
		if err != nil {
			mcpError(w, id, -32602, err.Error())
			return
		}
		if p.Name == "readme" {
			mcpResult(w, id, map[string]any{"isError": false, "content": []any{map[string]string{"type": "text", "text": mcpReadme}}})
			return
		}
		pool, err := a.store.Reader(r.Context(), acct)
		if err != nil {
			mcpError(w, id, -32603, "reader unavailable")
			return
		}
		res, err := runReadonly(r.Context(), a.cfg, pool, sqlRequest{Query: query, Limit: limit}, args...)
		if err != nil {
			mcpResult(w, id, map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}})
			return
		}
		body, _ := json.Marshal(res)
		mcpResult(w, id, map[string]any{"isError": false, "structuredContent": res, "content": []any{map[string]string{"type": "text", "text": string(body)}}})
	default:
		mcpError(w, id, -32601, "method not found")
	}
}

// Check Origin even when Sec-Fetch-Site is present. A configured public origin is
// authoritative behind TLS termination; otherwise use the actual request origin.
func mcpOrigin(cfg Config, r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if cfg.PublicURL != "" {
		public, err := url.Parse(cfg.PublicURL)
		if err != nil {
			return false
		}
		scheme, host = public.Scheme, public.Host
	}
	return u.Host == host && u.Scheme == scheme
}

func decodeMCP(dec *json.Decoder, v any) error {
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func validMCPID(id json.RawMessage) bool {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

func mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func mcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func mcpTools() []mcpTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": maxSQLLimit, "default": defaultSQLLimit}
	tool := func(name, desc string, props map[string]any, required ...string) mcpTool {
		if name != "readme" {
			props["limit"] = limit
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return mcpTool{name, desc, schema, map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}}
	}
	return append([]mcpTool{
		tool("readme", "Start here: Intelligence SQL guide, relation map, example queries for runs, events, agents and transcripts, and reviewed workflow proposals. All app state reads use query; proposals require browser approval to apply.", map[string]any{}),
		tool("schema_overview", "Find public tables and views with descriptions. Call describe_table for exact columns before writing SQL. Returns columns, rows, rowCount and truncated.", map[string]any{}),
		tool("describe_table", "Describe exact columns of one public table or view (unqualified name).", map[string]any{"table": str("Public relation name")}, "table"),
		tool("query", "Run one read-only SQL query as this account. Default 1000, maximum 10000 rows; 5-second default timeout. No writes or multiple statements.", map[string]any{"query": str("SQL; discover schema and columns first")}, "query"),
	}, proposalMCPTools()...)
}

func mcpQuery(name string, raw json.RawMessage) (string, []any, int, error) {
	// Validate against each tool's advertised properties, including numeric bounds, so typos
	// and wrong types fail rather than broadening a supposedly filtered read.
	var spec *mcpTool
	for _, t := range mcpTools() {
		if t.Name == name {
			spec = &t
			break
		}
	}
	bad := func(msg string) (string, []any, int, error) { return "", nil, 0, errors.New(msg) }
	if spec == nil {
		return bad("unknown tool")
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return bad("arguments must be an object")
	}
	props := spec.InputSchema["properties"].(map[string]any)
	for k, v := range fields {
		p, ok := props[k]
		if !ok {
			return bad("unknown argument: " + k)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return bad("null argument: " + k)
		}
		if p.(map[string]any)["type"] == "string" {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return bad("expected string: " + k)
			}
		} else {
			var n int
			if json.Unmarshal(v, &n) != nil || n < 0 || (k == "limit" && (n < 1 || n > maxSQLLimit)) {
				return bad("invalid integer: " + k)
			}
		}
	}
	if req, ok := spec.InputSchema["required"].([]string); ok {
		for _, k := range req {
			var s string
			_ = json.Unmarshal(fields[k], &s)
			if strings.TrimSpace(s) == "" {
				return bad("required argument: " + k)
			}
		}
	}
	var p struct {
		Query, Table string
		Limit        int
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return bad(err.Error())
	}
	if p.Limit == 0 {
		p.Limit = defaultSQLLimit
	}
	switch name {
	case "readme":
		return "", nil, 0, nil
	case "schema_overview":
		return `SELECT c.relname AS name, CASE c.relkind WHEN 'v' THEN 'view' ELSE 'table' END AS kind, obj_description(c.oid) AS description FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','v','m','p') ORDER BY c.relname`, nil, p.Limit, nil
	case "describe_table":
		return `SELECT column_name,data_type,udt_name,is_nullable,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 ORDER BY ordinal_position`, []any{p.Table}, p.Limit, nil
	case "query":
		return p.Query, nil, p.Limit, nil
	}
	return bad("unknown tool")
}
