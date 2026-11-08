// Package paseo speaks the Paseo daemon's WebSocket protocol (version 1), for the reads a dev box
// reports: the daemon's status, its workspaces and its agents, kept current by pushed updates.
//
// The protocol is the daemon's own and is not published as stable; this follows what the daemon
// sends and accepts, checked against Paseo 0.15. It reads only the fields Intelligence stores:
// agent records can carry credentials in fields it never decodes (persistence metadata).
package paseo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ProtocolVersion is the wire protocol this package speaks.
const ProtocolVersion = 1

// ErrRejected is returned when the daemon refuses the hello (bad credential, incompatible protocol).
var ErrRejected = errors.New("paseo daemon rejected the connection")

// Target is where and as whom to connect.
type Target struct {
	URL      string // ws://host:port/ws
	Token    string // the daemon's local credential
	ClientID string // a stable id for this client
}

// ServerInfo is what the daemon says about itself after the hello.
type ServerInfo struct {
	ServerID        string `json:"serverId"`
	Hostname        string `json:"hostname"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// DaemonStatus is daemon.get_status.
type DaemonStatus struct {
	Version   string     `json:"version"`
	Listen    string     `json:"listen"`
	PID       int        `json:"pid"`
	ServerID  string     `json:"serverId"`
	StartedAt time.Time  `json:"startedAt"`
	Providers []Provider `json:"providers"`
}

// Provider is an agent provider and whether the daemon can run it.
type Provider struct {
	Provider  string `json:"provider"`
	Available bool   `json:"available"`
	Error     string `json:"error"`
}

// Agent is the part of an agent snapshot Intelligence reads.
type Agent struct {
	ID                string                 `json:"id"`
	Title             string                 `json:"title"`
	Provider          string                 `json:"provider"`
	Model             string                 `json:"model"`
	RuntimeInfo       struct{ Model string } `json:"runtimeInfo"`
	ThinkingOptionID  string                 `json:"thinkingOptionId"`
	EffectiveThinking string                 `json:"effectiveThinkingOptionId"`
	Status            string                 `json:"status"` // running, idle, closed, error, ...
	Cwd               string                 `json:"cwd"`
	WorkspaceID       string                 `json:"workspaceId"`
	CreatedAt         time.Time              `json:"createdAt"`
	UpdatedAt         time.Time              `json:"updatedAt"`
	LastUserMessageAt time.Time              `json:"lastUserMessageAt"`
	ArchivedAt        time.Time              `json:"archivedAt"`
	RequiresAttention bool                   `json:"requiresAttention"`
	AttentionReason   string                 `json:"attentionReason"`
	Labels            map[string]string      `json:"labels"`
}

// ModelName is the model the agent runs: what its runtime reports, else what it was started with.
func (a Agent) ModelName() string {
	if a.RuntimeInfo.Model != "" && a.RuntimeInfo.Model != "default" {
		return a.RuntimeInfo.Model
	}
	return a.Model
}

// Thinking is the agent's effective thinking level.
func (a Agent) Thinking() string {
	if a.EffectiveThinking != "" {
		return a.EffectiveThinking
	}
	return a.ThinkingOptionID
}

// ParentAgentID is the agent that started this one, for sub-agents.
func (a Agent) ParentAgentID() string { return a.Labels["paseo.parent-agent-id"] }

// Workspace is the part of a workspace descriptor Intelligence reads. A workspace is a directory, a
// local checkout or a git worktree that agents run in.
type Workspace struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Title             string    `json:"title"`
	Kind              string    `json:"workspaceKind"` // directory, local_checkout, worktree
	Status            string    `json:"status"`        // running, attention, done
	Directory         string    `json:"workspaceDirectory"`
	Project           string    `json:"projectDisplayName"`
	ProjectRoot       string    `json:"projectRootPath"`
	Background        bool      `json:"background"`
	CreatedAt         time.Time `json:"createdAt"`
	ActivityAt        time.Time `json:"activityAt"`
	LastUserMessageAt time.Time `json:"lastUserMessageAt"`
	ArchivingAt       time.Time `json:"archivingAt"`
	GitRuntime        *struct {
		CurrentBranch string `json:"currentBranch"`
		IsDirty       bool   `json:"isDirty"`
	} `json:"gitRuntime"`
	DiffStat *struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"diffStat"`
}

// Branch is the workspace's checked-out branch, "" outside git.
func (w Workspace) Branch() string {
	if w.GitRuntime == nil {
		return ""
	}
	return w.GitRuntime.CurrentBranch
}

// Dirty reports uncommitted changes.
func (w Workspace) Dirty() bool { return w.GitRuntime != nil && w.GitRuntime.IsDirty }

// Additions and Deletions are the workspace's diff against its base.
func (w Workspace) Additions() int {
	if w.DiffStat == nil {
		return 0
	}
	return w.DiffStat.Additions
}

func (w Workspace) Deletions() int {
	if w.DiffStat == nil {
		return 0
	}
	return w.DiffStat.Deletions
}

// Event is one pushed update: exactly one field is set.
type Event struct {
	Agent            *Agent     // created or changed (archived agents arrive here with ArchivedAt set)
	AgentRemoved     string     // id of an agent gone from the daemon
	Workspace        *Workspace // created or changed
	WorkspaceRemoved string     // id of a workspace gone from the daemon
}

// Client is one connection to a daemon. Requests may run concurrently. Pushed updates arrive on
// Events; when the connection ends, Done closes and Err says why.
type Client struct {
	conn   *websocket.Conn
	info   ServerInfo
	events chan Event
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[string]chan reply
	done    chan struct{}
	err     error
}

type reply struct {
	payload json.RawMessage
	err     error
}

// Dial connects, sends the hello and waits for the daemon's server info.
func Dial(ctx context.Context, t Target) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, t.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", t.URL, err)
	}
	conn.SetReadLimit(64 << 20)
	hello := map[string]any{
		"type": "hello", "clientId": t.ClientID, "clientType": "cli", "protocolVersion": ProtocolVersion,
		"auth": map[string]any{"kind": "localCredential", "token": t.Token},
		// opt in to explicit subscriptions: otherwise every running agent's timeline is streamed
		"capabilities": map[string]bool{"hello_rejection": true, "owned_subscriptions": true,
			"explicit_event_subscriptions": true, "selective_agent_timeline": true, "all_providers": true},
	}
	if err := writeJSON(ctx, conn, hello); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("hello: %w", err)
	}
	c := &Client{conn: conn, events: make(chan Event, 256), pending: map[string]chan reply{}, done: make(chan struct{})}
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow()
			if s := websocket.CloseStatus(err); s >= 4000 {
				return nil, fmt.Errorf("%w: %v", ErrRejected, err)
			}
			return nil, fmt.Errorf("hello: %w", err)
		}
		var m struct {
			Type    string `json:"type"`
			Reason  string `json:"reason"`
			Message struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"message"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		if m.Type == "hello.rejected" {
			conn.CloseNow()
			return nil, fmt.Errorf("%w: %s", ErrRejected, m.Reason)
		}
		if m.Type == "session" && m.Message.Type == "status" {
			var info ServerInfo
			if err := json.Unmarshal(m.Message.Payload, &info); err == nil && info.ServerID != "" {
				c.info = info
				break
			}
		}
	}
	go c.read()
	return c, nil
}

// Info is the daemon's server info from the hello.
func (c *Client) Info() ServerInfo { return c.info }

// Events delivers pushed updates for the agents and workspaces this client fetched. It is buffered;
// a reader that falls far behind loses the connection rather than blocking the daemon.
func (c *Client) Events() <-chan Event { return c.events }

// Done closes when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection.
func (c *Client) Close() error {
	c.fail(errors.New("closed"))
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

// DaemonStatus reads the daemon's status and providers.
func (c *Client) DaemonStatus(ctx context.Context) (DaemonStatus, error) {
	var s DaemonStatus
	err := c.request(ctx, map[string]any{"type": "daemon.get_status.request"}, &s)
	return s, err
}

// pageLimit is how many entries each fetch page asks for.
const pageLimit = 200

type pageInfo struct {
	HasMore    bool   `json:"hasMore"`
	NextCursor string `json:"nextCursor"`
}

// Agents reads every unarchived agent and subscribes to their updates.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	err := c.fetchAll(ctx, "fetch_agents_request", "created_at", func(raw json.RawMessage) (pageInfo, error) {
		var p struct {
			Entries  []struct{ Agent Agent } `json:"entries"`
			PageInfo pageInfo                `json:"pageInfo"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return pageInfo{}, err
		}
		for _, e := range p.Entries {
			if e.Agent.ArchivedAt.IsZero() {
				out = append(out, e.Agent)
			}
		}
		return p.PageInfo, nil
	})
	return out, err
}

// Workspaces reads every workspace and subscribes to their updates.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var out []Workspace
	err := c.fetchAll(ctx, "fetch_workspaces_request", "name", func(raw json.RawMessage) (pageInfo, error) {
		var p struct {
			Entries  []Workspace `json:"entries"`
			PageInfo pageInfo    `json:"pageInfo"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return pageInfo{}, err
		}
		out = append(out, p.Entries...)
		return p.PageInfo, nil
	})
	return out, err
}

// fetchAll reads every page in sortKey order (each fetch has its own keys: agents created_at,
// workspaces name); only the first page opens a subscription.
func (c *Client) fetchAll(ctx context.Context, typ, sortKey string, page func(json.RawMessage) (pageInfo, error)) error {
	cursor := ""
	for first := true; ; first = false {
		req := map[string]any{"type": typ, "sort": []any{map[string]any{"key": sortKey, "direction": "asc"}}}
		p := map[string]any{"limit": pageLimit}
		if cursor != "" {
			p["cursor"] = cursor
		}
		req["page"] = p
		if first {
			req["subscribe"] = map[string]any{}
		}
		var raw json.RawMessage
		if err := c.request(ctx, req, &raw); err != nil {
			return err
		}
		info, err := page(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", typ, err)
		}
		if !info.HasMore || info.NextCursor == "" {
			return nil
		}
		cursor = info.NextCursor
	}
}

// request sends msg as a session request and decodes its response payload into out.
func (c *Client) request(ctx context.Context, msg map[string]any, out any) error {
	id := "i" + strconv.FormatUint(c.nextID.Add(1), 10)
	msg["requestId"] = id
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("paseo connection closed: %w", err)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := writeJSON(ctx, c.conn, map[string]any{"type": "session", "message": msg}); err != nil {
		return fmt.Errorf("%s: %w", msg["type"], err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", msg["type"], r.err)
		}
		return json.Unmarshal(r.payload, out)
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", msg["type"], ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s: paseo connection closed: %w", msg["type"], c.Err())
	}
}

// read dispatches the daemon's messages until the connection ends.
func (c *Client) read() {
	for {
		_, b, err := c.conn.Read(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &m) != nil || m.Type != "session" {
			continue
		}
		var head struct {
			RequestID string `json:"requestId"`
			Error     string `json:"error"`
			Kind      string `json:"kind"`
		}
		_ = json.Unmarshal(m.Message.Payload, &head)
		switch m.Message.Type {
		case "agent_update", "workspace_update":
			if e, ok := decodeEvent(m.Message.Type, head.Kind, m.Message.Payload); ok {
				select {
				case c.events <- e:
				default:
					c.fail(errors.New("update reader fell behind"))
					c.conn.CloseNow()
					return
				}
			}
		case "rpc_error":
			c.answer(head.RequestID, reply{err: errors.New(head.Error)})
		default:
			if head.RequestID != "" {
				c.answer(head.RequestID, reply{payload: m.Message.Payload})
			}
		}
	}
}

func decodeEvent(typ, kind string, payload json.RawMessage) (Event, bool) {
	var p struct {
		Agent     *Agent     `json:"agent"`
		AgentID   string     `json:"agentId"`
		Workspace *Workspace `json:"workspace"`
		ID        string     `json:"id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return Event{}, false
	}
	switch {
	case typ == "agent_update" && kind == "upsert" && p.Agent != nil:
		if !p.Agent.ArchivedAt.IsZero() {
			return Event{AgentRemoved: p.Agent.ID}, true
		}
		return Event{Agent: p.Agent}, true
	case typ == "agent_update" && kind == "remove" && p.AgentID != "":
		return Event{AgentRemoved: p.AgentID}, true
	case typ == "workspace_update" && kind == "upsert" && p.Workspace != nil:
		return Event{Workspace: p.Workspace}, true
	case typ == "workspace_update" && kind == "remove" && p.ID != "":
		return Event{WorkspaceRemoved: p.ID}, true
	}
	return Event{}, false
}

func (c *Client) answer(id string, r reply) {
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
