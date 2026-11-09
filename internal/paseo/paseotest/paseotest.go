// Package paseotest is a fake Paseo daemon that speaks the protocol's daemon side, for tests.
package paseotest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Daemon speaks the Paseo daemon's side of the protocol: hello, the three reads, paging,
// subscriptions and pushed updates. Everything it was sent is recorded.
type Daemon struct {
	t     *testing.T
	token string
	srv   *httptest.Server

	mu          sync.Mutex
	hellos      []map[string]any
	requests    []map[string]any
	conns       []*websocket.Conn
	agents      []map[string]any
	workspaces  []map[string]any
	projects    []map[string]any
	projectsErr string // project.list answers rpc_error with this
	statusErr   string // daemon.get_status answers rpc_error with this
	silent      bool   // never answers requests
	accepted    int
	timelines   map[string]timeline
	created     map[string]map[string]any // what each creation idempotency key made
	nextID      int

	schedules   []map[string]any  // schedules.go
	noSchedules bool              // every schedule request fails, as on a daemon without them
	runOnce     map[string]waiter // run-once requests waiting for their run to end, by run id

	catalog    []map[string]any // catalog.go
	catalogSet bool

	diffs map[string]diff // diff.go, by checkout directory
}

// FailProjects makes project.list answer rpc_error with msg ("" answers normally).
func (f *Daemon) FailProjects(msg string) { f.mu.Lock(); f.projectsErr = msg; f.mu.Unlock() }

// SetProjects sets the project catalog project.list answers with.
func (f *Daemon) SetProjects(projects ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = projects
}

type timeline struct {
	epoch   string
	entries []map[string]any
}

// SetTimeline sets agent's projected timeline: its epoch and entries (see Entry), in seq order.
func (f *Daemon) SetTimeline(agent, epoch string, entries ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timelines == nil {
		f.timelines = map[string]timeline{}
	}
	f.timelines[agent] = timeline{epoch, entries}
}

// Entry is a timeline entry covering seqs start..end, holding item (such as Message or ToolCall).
func Entry(start, end int, item map[string]any) map[string]any {
	return map[string]any{"provider": "claude", "item": item, "turnId": "turn-1", "timestamp": "2026-11-08T10:00:00.000Z",
		"seqStart": start, "seqEnd": end, "sourceSeqRanges": []any{}, "collapsed": []any{}}
}

// Message is a user_message, assistant_message or reasoning item.
func Message(kind, text string) map[string]any { return map[string]any{"type": kind, "text": text} }

// ToolCall is a tool_call item.
func ToolCall(name, status string) map[string]any {
	return map[string]any{"type": "tool_call", "callId": "call-" + name, "name": name, "status": status,
		"detail": map[string]any{"input": map[string]any{"command": "ls"}}, "error": nil}
}

// SetAgents sets the agent snapshots fetch_agents serves (see Agent).
func (f *Daemon) SetAgents(a ...map[string]any) { f.mu.Lock(); f.agents = a; f.mu.Unlock() }

// SetWorkspaces sets the workspace descriptors fetch_workspaces serves (see Workspace).
func (f *Daemon) SetWorkspaces(w ...map[string]any) { f.mu.Lock(); f.workspaces = w; f.mu.Unlock() }

// AgentStatus is an agent's status as the daemon has it ("" for none).
func (f *Daemon) AgentStatus(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.find(f.agents, id); i >= 0 {
		s, _ := f.agents[i]["status"].(string)
		return s
	}
	return ""
}

// FailStatus makes daemon.get_status answer rpc_error with msg ("" answers normally).
func (f *Daemon) FailStatus(msg string) { f.mu.Lock(); f.statusErr = msg; f.mu.Unlock() }

// Silence makes the daemon read requests and never answer them.
func (f *Daemon) Silence() { f.mu.Lock(); f.silent = true; f.mu.Unlock() }

// Hellos are the hello messages received.
func (f *Daemon) Hellos() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.hellos...)
}

// Connections is how many connections the daemon has accepted.
func (f *Daemon) Connections() int { f.mu.Lock(); defer f.mu.Unlock(); return f.accepted }

func New(t *testing.T, token string) *Daemon {
	t.Helper()
	f := &Daemon{t: t, token: token}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *Daemon) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws" }

func (f *Daemon) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ws" {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx := r.Context()
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.accepted++
	f.mu.Unlock()
	helloed := false
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			c.Close(4000, "bad json")
			return
		}
		if m["type"] == "hello" {
			f.mu.Lock()
			f.hellos = append(f.hellos, m)
			f.mu.Unlock()
			auth, _ := m["auth"].(map[string]any)
			if auth["kind"] != "localCredential" || auth["token"] != f.token {
				f.write(c, map[string]any{"type": "hello.rejected", "reason": "incorrect_password"})
				c.Close(4001, "Incorrect password")
				return
			}
			helloed = true
			f.write(c, session("status", map[string]any{"status": "server_info", "serverId": "srv_test", "hostname": "host-1",
				"version": "0.15.3", "protocolVersion": 1}))
			continue
		}
		if !helloed {
			c.Close(4002, "Session message before hello")
			return
		}
		msg, _ := m["message"].(map[string]any)
		f.mu.Lock()
		f.requests = append(f.requests, msg)
		silent := f.silent
		f.mu.Unlock()
		if silent {
			continue
		}
		f.answer(c, msg)
	}
}

func (f *Daemon) answer(c *websocket.Conn, msg map[string]any) {
	id, _ := msg["requestId"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !validFetch(msg) {
		// the daemon validates every request against its schema and answers a bad one this way
		f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "Unknown request, try upgrading the daemon"}))
		return
	}
	switch msg["type"] {
	case "agent_permission_response":
		response, _ := msg["response"].(map[string]any)
		if id == "" || msg["agentId"] == "" || response["behavior"] != "deny" || response["message"] == "" {
			f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "invalid permission response"}))
			return
		}
		f.write(c, session("agent_permission_resolved", map[string]any{"requestId": id, "agentId": msg["agentId"], "resolution": response}))
	case "project.list.request":
		if f.projectsErr != "" {
			f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": f.projectsErr, "code": "project_list_failed"}))
			return
		}
		f.write(c, session("project.list.response", map[string]any{"requestId": id, "projects": f.projects}))
	case "daemon.get_status.request":
		if f.statusErr != "" {
			f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": f.statusErr, "code": "boom"}))
			return
		}
		f.write(c, session("daemon.get_status.response", map[string]any{"requestId": id, "listen": "0.0.0.0:6767", "pid": 42,
			"serverId": "srv_test", "startedAt": "2026-11-08T07:00:00.000Z", "version": "0.15.3",
			"providers": []any{map[string]any{"provider": "claude", "available": true, "error": nil},
				map[string]any{"provider": "pi", "available": false, "error": "not installed"}}}))
	case "get_providers_snapshot_request":
		f.write(c, session("get_providers_snapshot_response", map[string]any{"requestId": id, "entries": f.catalogLocked(),
			"generatedAt": "2026-11-08T07:00:00.000Z"}))
	case "fetch_agents_request":
		entries, page := pageOf(f.agents, msg)
		for i := range entries {
			entries[i] = map[string]any{"agent": entries[i], "project": map[string]any{}}
		}
		f.write(c, session("fetch_agents_response", map[string]any{"requestId": id, "entries": entries, "pageInfo": page}))
	case "fetch_agent_timeline_request":
		f.write(c, session("fetch_agent_timeline_response", f.timelinePage(id, msg)))
	case "fetch_workspaces_request":
		entries, page := pageOf(f.workspaces, msg)
		f.write(c, session("fetch_workspaces_response", map[string]any{"requestId": id, "entries": entries, "pageInfo": page,
			"emptyProjects": []any{}}))
	default:
		if f.change(c, id, msg) || f.schedule(c, id, msg) || f.diff(c, id, msg) {
			return
		}
		f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "unknown request"}))
	}
}

// change answers the requests that change the daemon, as Paseo 0.15 does; false for any other.
// Created and archived workspaces and agents are pushed to every connection. Called with f.mu held.
func (f *Daemon) change(c *websocket.Conn, id string, msg map[string]any) bool {
	str := func(k string) string { s, _ := msg[k].(string); return s }
	key := str("idempotencyKey")
	switch msg["type"] {
	case "workspace.create.request":
		if w, ok := f.created[key]; ok && key != "" {
			f.write(c, session("workspace.create.response", map[string]any{"requestId": id, "workspace": w, "setupTerminalId": nil, "error": nil}))
			return true
		}
		src, _ := msg["source"].(map[string]any)
		dir, _ := src["path"].(string)
		kind := "directory"
		if src["kind"] == "worktree" {
			cwd, _ := src["cwd"].(string)
			branch, _ := src["branchName"].(string)
			dir, kind = cwd+"-worktrees/"+branch, "worktree"
		}
		if dir == "" {
			f.write(c, session("workspace.create.response", map[string]any{"requestId": id, "workspace": nil, "setupTerminalId": nil,
				"error": "A source path is required"}))
			return true
		}
		f.nextID++
		w := Workspace(fmt.Sprintf("wks_%016x", f.nextID), kind, "done")
		w["workspaceDirectory"] = dir
		if t := str("title"); t != "" {
			w["name"], w["title"] = t, t
		}
		f.workspaces = append(f.workspaces, w)
		f.remember(key, w)
		f.pushLocked("workspace_update", map[string]any{"kind": "upsert", "workspace": w})
		f.write(c, session("workspace.create.response", map[string]any{"requestId": id, "workspace": w, "setupTerminalId": nil, "error": nil}))
	case "agent.create.request":
		if a, ok := f.created[key]; ok && key != "" {
			f.write(c, session("agent.create.response", map[string]any{"requestId": id, "agent": a, "error": nil}))
			return true
		}
		cfg, _ := msg["config"].(map[string]any)
		provider, _ := cfg["provider"].(string)
		cwd, _ := cfg["cwd"].(string)
		if _, ok := msg["labels"].(map[string]any); !ok || cwd == "" {
			f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "Invalid message"}))
			return true
		}
		if !slices.Contains([]string{"claude", "codex", "opencode", "pi"}, provider) {
			f.write(c, session("agent.create.response", map[string]any{"requestId": id, "agent": nil,
				"error": "Unknown provider: " + provider}))
			return true
		}
		f.nextID++
		ws := str("workspaceId")
		if ws == "" {
			ws = "wks_for_" + cwd
		}
		a := Agent(fmt.Sprintf("agent-%d", f.nextID), ws, "running")
		a["cwd"], a["provider"], a["labels"] = cwd, provider, msg["labels"]
		if t, _ := cfg["title"].(string); t != "" {
			a["title"] = t
		}
		if m, _ := cfg["model"].(string); m != "" {
			a["model"] = m
		}
		f.agents = append(f.agents, a)
		f.remember(key, a)
		f.pushLocked("agent_update", map[string]any{"kind": "upsert", "agent": a, "project": map[string]any{}})
		f.write(c, session("agent.create.response", map[string]any{"requestId": id, "agent": a, "error": nil}))
	case "archive_workspace_request":
		ws := str("workspaceId")
		i := f.find(f.workspaces, ws)
		if i < 0 {
			f.write(c, session("archive_workspace_response", map[string]any{"requestId": id, "workspaceId": ws, "archivedAt": nil,
				"error": "Workspace not found: " + ws}))
			return true
		}
		f.workspaces = slices.Delete(f.workspaces, i, i+1)
		f.pushLocked("workspace_update", map[string]any{"kind": "remove", "id": ws})
		f.write(c, session("archive_workspace_response", map[string]any{"requestId": id, "workspaceId": ws,
			"archivedAt": "2026-11-08T12:00:00.000Z", "error": nil}))
	case "workspace.title.set.request":
		ws := str("workspaceId")
		i := f.find(f.workspaces, ws)
		if i < 0 {
			f.write(c, session("workspace.title.set.response", map[string]any{"requestId": id, "workspaceId": ws, "accepted": false,
				"title": nil, "error": "Workspace not found"}))
			return true
		}
		f.workspaces[i]["title"] = msg["title"]
		f.pushLocked("workspace_update", map[string]any{"kind": "upsert", "workspace": f.workspaces[i]})
		f.write(c, session("workspace.title.set.response", map[string]any{"requestId": id, "workspaceId": ws, "accepted": true,
			"title": msg["title"], "error": nil}))
	case "update_agent_request", "send_agent_message_request", "cancel_agent_request", "archive_agent_request":
		ag := str("agentId")
		i := f.find(f.agents, ag)
		if i < 0 {
			// the daemon throws for an unknown agent, which reaches the client as an rpc_error
			f.write(c, session("rpc_error", map[string]any{"requestId": id, "requestType": msg["type"],
				"error": "Request failed: Agent not found: " + ag, "code": "handler_error"}))
			return true
		}
		switch msg["type"] {
		case "update_agent_request":
			f.agents[i]["title"] = msg["name"]
			f.write(c, session("update_agent_response", map[string]any{"requestId": id, "agentId": ag, "ok": true, "error": nil}))
		case "send_agent_message_request":
			if b, ok := msg["activeTurnBehavior"]; ok && b != "steer" && b != "interrupt" { // the daemon's schema refuses it
				f.write(c, session("rpc_error", map[string]any{"requestId": id, "requestType": msg["type"],
					"error": "Invalid message", "code": "invalid_message"}))
				return true
			}
			// a message starts a turn (or joins the running one)
			f.agents[i]["status"] = "running"
			f.pushLocked("agent_update", map[string]any{"kind": "upsert", "agent": f.agents[i]})
			f.write(c, session("send_agent_message_response", map[string]any{"requestId": id, "agentId": ag, "accepted": true, "error": nil}))
		case "cancel_agent_request":
			f.write(c, session("cancel_agent_response", map[string]any{"requestId": id, "agentId": ag, "agent": f.agents[i], "error": nil}))
		case "archive_agent_request":
			f.agents = slices.Delete(f.agents, i, i+1)
			f.pushLocked("agent_update", map[string]any{"kind": "remove", "agentId": ag})
			f.write(c, session("agent_archived", map[string]any{"requestId": id, "agentId": ag, "archivedAt": "2026-11-08T12:00:00.000Z"}))
		}
	default:
		return false
	}
	return true
}

func (f *Daemon) remember(key string, v map[string]any) {
	if key == "" {
		return
	}
	if f.created == nil {
		f.created = map[string]map[string]any{}
	}
	f.created[key] = v
}

func (f *Daemon) find(items []map[string]any, id string) int {
	return slices.IndexFunc(items, func(m map[string]any) bool { return m["id"] == id })
}

func (f *Daemon) pushLocked(typ string, payload map[string]any) {
	for _, c := range f.conns {
		f.write(c, session(typ, payload))
	}
}

// pageOf serves items in pages of the request's limit, with the next index as the cursor.
func pageOf(items []map[string]any, msg map[string]any) ([]map[string]any, map[string]any) {
	page, _ := msg["page"].(map[string]any)
	limit := len(items)
	if l, ok := page["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}
	start := 0
	if c, ok := page["cursor"].(string); ok {
		start, _ = strconv.Atoi(c)
	}
	end := min(start+limit, len(items))
	out := append([]map[string]any(nil), items[start:end]...)
	info := map[string]any{"hasMore": end < len(items), "nextCursor": nil, "prevCursor": nil}
	if end < len(items) {
		info["nextCursor"] = strconv.Itoa(end)
	}
	return out, info
}

func (f *Daemon) write(c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
}

// Push sends a session message (agent_update, workspace_update, ...) to every connection.
func (f *Daemon) Push(typ string, payload map[string]any) {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		f.write(c, session(typ, payload))
	}
}

// DropAll closes every connection, as a daemon restart does.
func (f *Daemon) DropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close(websocket.StatusGoingAway, "restarting")
	}
	f.conns = nil
}

// Requests are the session requests of one type received, in order.
func (f *Daemon) Requests(typ string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.requests {
		if r["type"] == typ {
			out = append(out, r)
		}
	}
	return out
}

func session(typ string, payload map[string]any) map[string]any {
	return map[string]any{"type": "session", "message": map[string]any{"type": typ, "payload": payload}}
}

// Agent is an agent snapshot as the daemon sends it, in workspace, with a field (persistence
// metadata) a reader must never copy.
func Agent(id, workspace, status string) map[string]any {
	return map[string]any{"id": id, "title": "Agent " + id, "provider": "claude", "model": "claude-opus-5-5",
		"runtimeInfo": map[string]any{"model": nil, "provider": "claude"}, "thinkingOptionId": "medium",
		"effectiveThinkingOptionId": "medium", "status": status, "cwd": "/w/" + workspace, "workspaceId": workspace,
		"createdAt": "2026-11-08T10:00:00.000Z", "updatedAt": "2026-11-08T11:00:00.000Z",
		"lastUserMessageAt": "2026-11-08T10:30:00.000Z", "archivedAt": nil, "requiresAttention": false,
		"attentionReason": nil, "labels": map[string]any{"paseo.parent-agent-id": "parent-1", "secret-looking": "x"},
		"persistence": map[string]any{"metadata": map[string]any{"daemonAppendSystemPrompt": "never copied"}}}
}

// Workspace is a workspace descriptor as the daemon sends it.
func Workspace(id, kind, status string) map[string]any {
	return map[string]any{"id": id, "name": "ws " + id, "title": nil, "workspaceKind": kind, "status": status,
		"workspaceDirectory": "/w/" + id, "projectDisplayName": "example/app", "projectRootPath": "/w/app",
		"createdAt": "2026-11-08T09:00:00.000Z", "activityAt": nil, "lastUserMessageAt": "2026-11-08T10:30:00.000Z",
		"archivingAt": nil, "background": false,
		"gitRuntime": map[string]any{"currentBranch": "feature/x", "isDirty": true}, "diffStat": map[string]any{"additions": 12, "deletions": 3}}
}

// validFetch checks a fetch request's sort keys and page limit as the daemon's schema does.
func validFetch(msg map[string]any) bool {
	keys := map[string][]string{
		"fetch_agents_request":     {"status_priority", "created_at", "updated_at", "title"},
		"fetch_workspaces_request": {"status_priority", "activity_at", "name", "project_id"},
	}[fmt.Sprint(msg["type"])]
	if keys == nil {
		return true
	}
	sorts, _ := msg["sort"].([]any)
	for _, s := range sorts {
		k := fmt.Sprint(s.(map[string]any)["key"])
		if !slices.Contains(keys, k) {
			return false
		}
	}
	if page, ok := msg["page"].(map[string]any); ok {
		if l, _ := page["limit"].(float64); l > 200 || l < 1 {
			return false
		}
	}
	return true
}

// timelinePage answers a timeline fetch: tail (everything) or the entries starting after a cursor.
// A cursor from another epoch is stale: the daemon answers with the whole timeline and reset set.
func (f *Daemon) timelinePage(id string, msg map[string]any) map[string]any {
	agent, _ := msg["agentId"].(string)
	tl, ok := f.timelines[agent]
	if !ok {
		e := "agent not found"
		return map[string]any{"requestId": id, "agentId": agent, "entries": []any{}, "error": e, "epoch": "",
			"window": map[string]any{"minSeq": 0, "maxSeq": 0, "nextSeq": 0}}
	}
	after, reset := -1, false
	if cur, ok := msg["cursor"].(map[string]any); ok && msg["direction"] == "after" {
		if cur["epoch"] != tl.epoch {
			reset = true
		} else {
			after = int(cur["seq"].(float64))
		}
	}
	entries := []any{}
	for _, e := range tl.entries {
		if after < 0 || e["seqStart"].(int) > after {
			entries = append(entries, e)
		}
	}
	return map[string]any{"requestId": id, "agentId": agent, "agent": nil, "direction": msg["direction"], "projection": "projected",
		"epoch": tl.epoch, "reset": reset, "staleCursor": reset, "gap": false, "hasOlder": false, "hasNewer": false,
		"window": map[string]any{"minSeq": 1, "maxSeq": 1, "nextSeq": 2}, "startCursor": nil, "endCursor": nil,
		"entries": entries, "error": nil}
}
