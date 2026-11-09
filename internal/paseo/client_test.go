package paseo_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

const token = "tok_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func dial(t *testing.T, f *paseotest.Daemon) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Target{URL: f.URL(), Token: token, ClientID: "intelligence-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestHelloAsksOnlyForWhatItSubscribesTo(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if info := c.Info(); info.ServerID != "srv_test" || info.Hostname != "host-1" || info.Version != "0.15.3" {
		t.Errorf("server info = %+v", info)
	}
	h := f.Hellos()[0]
	if h["protocolVersion"] != float64(1) || h["clientId"] != "intelligence-test" || h["clientType"] != "cli" {
		t.Errorf("hello = %v", h)
	}
	caps, _ := h["capabilities"].(map[string]any)
	// without these the daemon streams every running agent's whole timeline to the client
	for _, k := range []string{"selective_agent_timeline", "owned_subscriptions", "explicit_event_subscriptions", "hello_rejection"} {
		if caps[k] != true {
			t.Errorf("hello capability %s = %v, want true", k, caps[k])
		}
	}
}

func TestWrongTokenIsAnAuthError(t *testing.T) {
	f := paseotest.New(t, token)
	_, err := Dial(ctx5(t), Target{URL: f.URL(), Token: "wrong", ClientID: "x"})
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "incorrect_password") {
		t.Fatalf("err = %v, want ErrRejected naming the reason", err)
	}
}

func TestDaemonStatus(t *testing.T) {
	c := dial(t, paseotest.New(t, token))
	s, err := c.DaemonStatus(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "0.15.3" || s.PID != 42 || s.Listen != "0.0.0.0:6767" || s.StartedAt.IsZero() || len(s.Providers) != 2 {
		t.Fatalf("status = %+v", s)
	}
	if p := s.Providers[1]; p.Provider != "pi" || p.Available || p.Error != "not installed" {
		t.Errorf("provider = %+v", p)
	}
}

func TestRPCErrorsComeBackAsErrors(t *testing.T) {
	f := paseotest.New(t, token)
	f.FailStatus("status exploded")
	c := dial(t, f)
	if _, err := c.DaemonStatus(ctx5(t)); err == nil || !strings.Contains(err.Error(), "status exploded") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestsTimeOutWithTheirContext(t *testing.T) {
	f := paseotest.New(t, token)
	f.Silence()
	c := dial(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.DaemonStatus(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
}

func TestAgentsReadsEveryPageAndSubscribes(t *testing.T) {
	f := paseotest.New(t, token)
	var snaps []map[string]any
	for i := range 450 {
		snaps = append(snaps, paseotest.Agent(fmt.Sprintf("a%03d", i), "w1", "idle"))
	}
	f.SetAgents(snaps...)
	c := dial(t, f)
	agents, err := c.Agents(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 450 || agents[449].ID != "a449" {
		t.Fatalf("got %d agents", len(agents))
	}
	reqs := f.Requests("fetch_agents_request")
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3 pages of 200", len(reqs))
	}
	if _, ok := reqs[0]["subscribe"]; !ok {
		t.Errorf("first page does not subscribe to agent updates")
	}
	for _, r := range reqs[1:] {
		if _, ok := r["subscribe"]; ok {
			t.Errorf("later pages must not open more subscriptions")
		}
	}
}

func TestAgentFieldsAreReadAndPrivateOnesAreNot(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetAgents(paseotest.Agent("a1", "w1", "running"))
	c := dial(t, f)
	agents, err := c.Agents(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	a := agents[0]
	if a.ID != "a1" || a.Title != "Agent a1" || a.Provider != "claude" || a.ModelName() != "claude-opus-5-5" ||
		a.Thinking() != "medium" || a.Status != "running" || a.WorkspaceID != "w1" || a.Cwd != "/w/w1" ||
		a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.LastUserMessageAt.IsZero() || a.ParentAgentID() != "parent-1" {
		t.Errorf("agent = %+v", a)
	}
	if strings.Contains(fmt.Sprintf("%+v", a), "never copied") {
		t.Errorf("agent kept its persistence metadata, which can hold credentials")
	}
}

func TestWorkspacesReadEveryPage(t *testing.T) {
	f := paseotest.New(t, token)
	var descs []map[string]any
	for i := range 250 {
		descs = append(descs, paseotest.Workspace(fmt.Sprintf("w%03d", i), "worktree", "running"))
	}
	f.SetWorkspaces(descs...)
	c := dial(t, f)
	ws, err := c.Workspaces(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 250 {
		t.Fatalf("got %d", len(ws))
	}
	w := ws[0]
	if w.ID != "w000" || w.Name != "ws w000" || w.Kind != "worktree" || w.Status != "running" || w.Directory != "/w/w000" ||
		w.Project != "example/app" || w.Branch() != "feature/x" || !w.Dirty() || w.Additions() != 12 || w.Deletions() != 3 ||
		w.CreatedAt.IsZero() || !w.ActivityAt.IsZero() {
		t.Errorf("workspace = %+v", w)
	}
	if _, ok := f.Requests("fetch_workspaces_request")[0]["subscribe"]; !ok {
		t.Errorf("workspaces are not subscribed to")
	}
}

func TestPushedUpdatesArriveAsEvents(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if _, err := c.Agents(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	f.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a9", "w1", "running")})
	f.Push("agent_update", map[string]any{"kind": "remove", "agentId": "a8"})
	f.Push("workspace_update", map[string]any{"kind": "upsert", "workspace": paseotest.Workspace("w9", "local_checkout", "done")})
	f.Push("workspace_update", map[string]any{"kind": "remove", "id": "w8"})
	f.Push("agent_stream", map[string]any{"agentId": "a9"}) // not an update: ignored
	var got []string
	for len(got) < 4 {
		select {
		case e := <-c.Events():
			switch {
			case e.Agent != nil:
				got = append(got, "agent+"+e.Agent.ID)
			case e.AgentRemoved != "":
				got = append(got, "agent-"+e.AgentRemoved)
			case e.Workspace != nil:
				got = append(got, "ws+"+e.Workspace.ID)
			case e.WorkspaceRemoved != "":
				got = append(got, "ws-"+e.WorkspaceRemoved)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("events so far %v", got)
		}
	}
	if strings.Join(got, " ") != "agent+a9 agent-a8 ws+w9 ws-w8" {
		t.Errorf("events = %v", got)
	}
}

func TestADroppedConnectionEndsTheClient(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	f.DropAll()
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client did not notice the dropped connection")
	}
	if c.Err() == nil {
		t.Errorf("Err() is nil after the connection dropped")
	}
	if _, err := c.DaemonStatus(ctx5(t)); err == nil {
		t.Errorf("a request on a dropped connection should fail at once")
	}
}

func TestLocalTargetReadsThePaseoHome(t *testing.T) {
	home := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("paseo.pid", `{"pid": 1, "listen": "0.0.0.0:6767", "hostname": "host-1"}`, 0o644)
	write("local-credential", token+"\n", 0o600)
	tgt, err := LocalTarget(home, "intelligence-host")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.URL != "ws://127.0.0.1:6767/ws" || tgt.Token != token || tgt.ClientID != "intelligence-host" {
		t.Errorf("target = %+v (an all-interfaces listen is dialed on loopback)", tgt)
	}
	write("paseo.pid", `{"pid": 1, "listen": "192.0.2.9:7000"}`, 0o644)
	if tgt, _ := LocalTarget(home, "x"); tgt.URL != "ws://192.0.2.9:7000/ws" {
		t.Errorf("url = %q", tgt.URL)
	}
	write("paseo.pid", `{"pid": 1, "listen": "/run/paseo.sock"}`, 0o644)
	if _, err := LocalTarget(home, "x"); err == nil {
		t.Errorf("a unix socket listen is not supported yet and should say so")
	}
}

func TestLocalTargetExplainsAMissingDaemon(t *testing.T) {
	if _, err := LocalTarget(t.TempDir(), "x"); err == nil || !strings.Contains(err.Error(), "paseo.pid") {
		t.Fatalf("err = %v", err)
	}
}

func TestTimelineReadsWholeThenAfterACursor(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetTimeline("a1", "ep1",
		paseotest.Entry(1, 1, paseotest.Message("user_message", "fix the login")),
		paseotest.Entry(2, 5, paseotest.Message("assistant_message", "on it")),
		paseotest.Entry(6, 6, paseotest.ToolCall("Bash", "completed")))
	c := dial(t, f)
	tl, err := c.Timeline(ctx5(t), "a1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Epoch != "ep1" || tl.Reset || len(tl.Entries) != 3 {
		t.Fatalf("timeline = %+v", tl)
	}
	e := tl.Entries[1]
	if e.SeqStart != 2 || e.SeqEnd != 5 || e.TurnID != "turn-1" || e.Provider != "claude" || e.Timestamp.IsZero() ||
		!strings.Contains(string(e.Item), `"on it"`) {
		t.Errorf("entry = %+v", e)
	}
	req := f.Requests("fetch_agent_timeline_request")[0]
	if req["direction"] != "tail" || req["limit"] != float64(0) {
		t.Errorf("first read = %v, want the whole tail (limit 0 is everything)", req)
	}

	tl, err = c.Timeline(ctx5(t), "a1", &Cursor{Epoch: "ep1", Seq: 5})
	if err != nil {
		t.Fatal(err)
	}
	if tl.Reset || len(tl.Entries) != 1 || tl.Entries[0].SeqStart != 6 {
		t.Fatalf("after seq 5 = %+v", tl)
	}
}

func TestTimelineFromAnotherEpochIsAWholeReset(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetTimeline("a1", "ep2", paseotest.Entry(1, 1, paseotest.Message("user_message", "again")))
	c := dial(t, f)
	tl, err := c.Timeline(ctx5(t), "a1", &Cursor{Epoch: "ep1", Seq: 40})
	if err != nil {
		t.Fatal(err)
	}
	if !tl.Reset || tl.Epoch != "ep2" || len(tl.Entries) != 1 {
		t.Fatalf("timeline = %+v, want a reset with the whole new timeline", tl)
	}
}

func TestTimelineErrorsComeBack(t *testing.T) {
	c := dial(t, paseotest.New(t, token))
	if _, err := c.Timeline(ctx5(t), "nope", nil); err == nil || !strings.Contains(err.Error(), "agent not found") {
		t.Fatalf("err = %v", err)
	}
}

// creation writes what the daemon keeps of one workspace creation in home/creations: the receipt,
// named for the creation's identity, and the claim on the workspace id, which names the receipt.
func creation(t *testing.T, home, workspaceID, key string) {
	t.Helper()
	dir := filepath.Join(home, "creations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := "4b1c" + strings.Repeat("0", 60)
	receipt := `{"fingerprint":"f","inFlight":null,"snapshot":{"kind":"workspace","idempotencyKey":"` + key +
		`","phase":"completed","workspaceId":"` + workspaceID + `"}}`
	// sha256 of the JSON ["workspace","<id>"], as the daemon names a workspace's claim
	sum := sha256.Sum256([]byte(`["workspace","` + workspaceID + `"]`))
	for name, body := range map[string]string{identity + ".json": receipt, hex.EncodeToString(sum[:]) + ".claim": identity} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreationKeyReadsTheDaemonsReceipt(t *testing.T) {
	home := t.TempDir()
	creation(t, home, "wks_5d8ec80b0643cc24", "draft_msg_1794182094800_sw78to36g")
	key, err := CreationKey(home, "wks_5d8ec80b0643cc24")
	if err != nil || key != "draft_msg_1794182094800_sw78to36g" {
		t.Errorf("key = %q, %v", key, err)
	}
	// a workspace made before the daemon kept receipts, or opened rather than created, has none
	if key, err := CreationKey(home, "wks_0000000000000000"); err != nil || key != "" {
		t.Errorf("no receipt: key = %q, %v", key, err)
	}
	if key, err := CreationKey(t.TempDir(), "wks_5d8ec80b0643cc24"); err != nil || key != "" {
		t.Errorf("no creations dir: key = %q, %v", key, err)
	}
}

func TestCreatedInTheApp(t *testing.T) {
	for key, want := range map[string]bool{
		"draft_msg_1794182094800_sw78to36g":    true,  // the app's composer
		"0c8e2b1e-5a6f-4c55-9d7e-0d6a3f2b9c11": false, // CLI, MCP
		"intelligence.command=42":              false,
		"":                                     false,
	} {
		if got := CreatedInApp(key); got != want {
			t.Errorf("CreatedInApp(%q) = %v", key, got)
		}
	}
}
