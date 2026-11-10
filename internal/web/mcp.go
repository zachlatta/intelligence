package web

import (
	"bytes"
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
// credential validation uses the account store; every tool reads through its account's pool.
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
	acct, err := a.store.MCPAccount(r.Context(), token)
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
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && v != mcpVersion {
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
		mcpResult(w, id, map[string]any{"protocolVersion": mcpVersion, "serverInfo": map[string]string{"name": "intelligence", "version": a.cfg.Version}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": "Read-only Intelligence data, scoped to the token's account. Start with schema and describe_table before SQL. Transcripts are synced snapshots, not all historical sessions; check hosts.last_seen and transcripts.synced_at. Page transcript seq_start and run event IDs; a changed transcript epoch requires starting over."})
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
		query, args, limit, err := mcpQuery(p.Name, p.Arguments)
		if err != nil {
			mcpError(w, id, -32602, err.Error())
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
	integer := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "description": desc}
	}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": maxSQLLimit, "default": defaultSQLLimit}
	tool := func(name, desc string, props map[string]any, required ...string) mcpTool {
		props["limit"] = limit
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return mcpTool{name, desc, schema, map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}}
	}
	return []mcpTool{
		tool("schema", "List public tables and views with descriptions. All data tools return SQL columns, rows, rowCount and truncated.", map[string]any{}),
		tool("describe_table", "Describe exact columns of one public table or view (unqualified name).", map[string]any{"table": str("Public relation name")}, "table"),
		tool("query", "Run one read-only SQL query as this account. Default 1000, maximum 10000 rows; 5-second default timeout. No writes or multiple statements.", map[string]any{"query": str("SQL; discover schema and columns first")}, "query"),
		tool("list_agents", "Find live and retained workflow agents by optional host and title substring. Sorted by creation time, newest first. Use query for other filters.", map[string]any{"host": str("Exact host, optional"), "search": str("Title substring, optional")}),
		tool("read_transcript", "Read synced transcript entries with epoch and sync time. Empty means absent/not synced or no entries at this cursor. Next page: afterSeq = last seq_start. Restart on epoch change. Archived agents may no longer have transcripts.", map[string]any{"host": str("Exact host"), "agentId": str("Exact agent ID"), "afterSeq": integer("Exclusive seq_start cursor; omit for beginning")}, "host", "agentId"),
		tool("list_runs", "List workflow runs, newest first, optionally filtered by status. Includes workflow, step, orchestrator and waiting note. Use query for workflows, definitions and step timings.", map[string]any{"status": str("Exact run status, optional")}),
		tool("read_run", "Read one workflow run's events in order, with current run state in each row. Next page: afterEvent = last event_id. For a run with no events use query on workflow_runs.", map[string]any{"runId": str("Workflow run UUID"), "afterEvent": integer("Exclusive event ID cursor; default 0")}, "runId"),
	}
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
		Query, Table, Host, Search, AgentID, RunID, Status string
		Limit                                              int
		AfterSeq                                           *int
		AfterEvent                                         int
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return bad(err.Error())
	}
	if p.Limit == 0 {
		p.Limit = defaultSQLLimit
	}
	switch name {
	case "schema":
		return `SELECT c.relname AS name, CASE c.relkind WHEN 'v' THEN 'view' ELSE 'table' END AS kind, obj_description(c.oid) AS description FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','v','m','p') ORDER BY c.relname`, nil, p.Limit, nil
	case "describe_table":
		return `SELECT column_name,data_type,udt_name,is_nullable,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 ORDER BY ordinal_position`, []any{p.Table}, p.Limit, nil
	case "query":
		return p.Query, nil, p.Limit, nil
	case "list_agents":
		return `SELECT host,id,workspace_id,title,status,provider,model,created_at FROM recorded_agents WHERE ($1='' OR host=$1) AND ($2='' OR strpos(lower(title),lower($2))>0) ORDER BY created_at DESC,host,id`, []any{p.Host, p.Search}, p.Limit, nil
	case "read_transcript":
		after := -1
		if p.AfterSeq != nil {
			after = *p.AfterSeq
		}
		return `SELECT t.epoch,t.synced_at,e.seq_start,e.seq_end,e.at,e.kind,e.item FROM transcripts t JOIN transcript_entries e USING(host,agent_id) WHERE t.host=$1 AND t.agent_id=$2 AND e.seq_start>$3 ORDER BY e.seq_start`, []any{p.Host, p.AgentID, after}, p.Limit, nil
	case "list_runs":
		return `SELECT * FROM workflow_runs WHERE ($1='' OR status=$1) ORDER BY created_at DESC,id`, []any{p.Status}, p.Limit, nil
	case "read_run":
		return `SELECT r.id,r.status,r.step,r.note,r.error,r.orchestrator_id,r.host,e.id AS event_id,e.at,e.kind,e.step AS event_step,e.to_step,e.text FROM workflow_runs r JOIN workflow_run_events e ON e.run_id=r.id WHERE r.id::text=$1 AND e.id>$2 ORDER BY e.id`, []any{p.RunID, p.AfterEvent}, p.Limit, nil
	}
	return bad("unknown tool")
}
