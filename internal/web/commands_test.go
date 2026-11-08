package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// cmdServer has host-1 reported, in the signed-in account.
func cmdServer(t *testing.T) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	h := instance(t, d, d.Owner)
	sendReport(t, h, sampleReport)
	return h, d
}

// instance is one server instance on d's database, through p.
func instance(t *testing.T, d dbtest.DB, p *pgxpool.Pool) http.Handler {
	return newServer(t, d, p, "v")
}

// TestCommandsCrossServerInstances: during a deploy two instances run. A command queued on one
// goes at once down a stream held by the other, and a wait on one returns as soon as the host's
// result lands through the other. Neither polls.
func TestCommandsCrossServerInstances(t *testing.T) {
	h, d := cmdServer(t)
	other := instance(t, d, dbtest.Pool(t, d))
	b := openStream(t, h, "host-1")
	start := time.Now()
	_, c, _ := enqueue(t, other, "host-1", "", archiveA1)
	if got := b.command(); got.ID != c.ID {
		t.Fatalf("command = %+v", got)
	}
	done := make(chan fleet.Command)
	go func() { _, c := showCmd(t, other, "host-1", c.ID, "?wait=30s"); done <- c }()
	time.Sleep(100 * time.Millisecond) // let the wait start
	b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandSucceeded}})
	select {
	case fin := <-done:
		if fin.Status != hostclient.CommandSucceeded {
			t.Errorf("finished = %+v", fin)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("?wait never returned")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s: something waited on a poll", took)
	}
}

func enqueue(t *testing.T, h http.Handler, host, token, body string) (int, fleet.Command, string) {
	t.Helper()
	rec := post(t, h, "/api/hosts/"+host+"/commands", token, []byte(body))
	var c fleet.Command
	if rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if loc := rec.Header().Get("Location"); loc != "/api/hosts/"+host+"/commands/"+c.ID {
			t.Errorf("location = %q", loc)
		}
	}
	return rec.Code, c, rec.Body.String()
}

func showCmd(t *testing.T, h http.Handler, host, id, query string) (int, fleet.Command) {
	t.Helper()
	rec := get(t, h, "/api/hosts/"+host+"/commands/"+id+query, nil)
	var c fleet.Command
	if rec.Code == http.StatusOK {
		json.Unmarshal(rec.Body.Bytes(), &c)
	}
	return rec.Code, c
}

const archiveA1 = `{"kind":"archive_agent","args":{"agentId":"a1"}}`

// TestCommandsGoOnlyToTheAccountsOwnHosts: another account can neither queue a command for a host
// that is not its own nor read one.
func TestCommandsGoOnlyToTheAccountsOwnHosts(t *testing.T) {
	h, _ := cmdServer(t)
	_, c, _ := enqueue(t, h, "host-1", "", archiveA1)
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	if rec := call(t, h, http.MethodPost, "/api/hosts/host-1/commands", archiveA1, nil, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob queues for someone's host: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodGet, "/api/hosts/host-1/commands/"+c.ID, "", nil, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob reads someone's command: %d", rec.Code)
	}
	if rec := call(t, anon(h), http.MethodGet, "/api/hosts/host-1/commands/"+c.ID, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no one reads a command: %d", rec.Code)
	}
}

func TestCommandsAreChecked(t *testing.T) {
	h, _ := cmdServer(t)
	for body, want := range map[string]int{
		`{"kind":"explode"}`:                                           http.StatusBadRequest,
		`{"kind":"send_message","args":{"agentId":"a1"}}`:              http.StatusBadRequest,
		`{"kind":"archive_agent","args":{"agentId":"a1"},"ttl":"1y"}`:  http.StatusBadRequest,
		`{"kind":"archive_agent","args":{"agentId":"a1"},"ttl":"48h"}`: http.StatusBadRequest,
		`{"kind":"archive_agent","args":{"agentId":"a1"},"extra":1}`:   http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if code, _, msg := enqueue(t, h, "host-1", "", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, msg, want)
		}
	}
	if code, _, _ := enqueue(t, h, "nobody", "", archiveA1); code != http.StatusNotFound {
		t.Errorf("unknown host: %d, want 404", code)
	}
	if code, _ := showCmd(t, h, "host-1", "not-an-id", ""); code != http.StatusNotFound {
		t.Errorf("unknown command: %d", code)
	}
}

func TestACommandGoesDownTheStreamAtOnceAndItsResultComesBack(t *testing.T) {
	h, _ := cmdServer(t)
	b := openStream(t, h, "host-1")
	code, c, msg := enqueue(t, h, "host-1", "", `{"kind":"create_agent","args":{"cwd":"/w/app","provider":"claude","prompt":"go"},"ttl":"1m"}`)
	if code != http.StatusAccepted || c.Status != hostclient.CommandQueued || !c.ExpiresAt.Equal(c.CreatedAt.Add(time.Minute)) {
		t.Fatalf("enqueue: %d %s", code, msg)
	}
	got := b.command()
	if got.ID != c.ID || got.Kind != hostclient.CreateAgentKind || !strings.Contains(string(got.Args), `"prompt":"go"`) {
		t.Fatalf("command = %+v", got)
	}
	if code, other := showCmd(t, h, "host-2", c.ID, ""); code != http.StatusNotFound {
		t.Errorf("another host's path: %d %+v", code, other)
	}
	_, sent := showCmd(t, h, "host-1", c.ID, "")
	if sent.Status != hostclient.CommandSent || sent.Attempts != 1 {
		t.Errorf("after sending: %+v", sent)
	}
	done := make(chan fleet.Command)
	go func() { _, c := showCmd(t, h, "host-1", c.ID, "?wait=5s"); done <- c }()
	b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandRunning}})
	b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandSucceeded,
		Result: json.RawMessage(`{"agentId":"a9"}`)}})
	select {
	case fin := <-done:
		if fin.Status != hostclient.CommandSucceeded || string(fin.Result) != `{"agentId":"a9"}` || fin.StartedAt.IsZero() {
			t.Errorf("finished = %+v", fin)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("?wait never returned")
	}
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: "00000000-0000-4000-8000-000000000000",
		Status: hostclient.CommandFailed}}); ack.Error != "" {
		t.Errorf("an update for an unknown command is dropped, not refused: %+v", ack)
	}
}

func TestQueuedCommandsWaitForTheHostAndUnfinishedOnesAreResent(t *testing.T) {
	d := dbtest.New(t)
	h := instance(t, d, d.Owner)
	key := keyOf(t, h, "host-1")                                                             // the account's
	if err := fleet.New(d.Owner).Record(t.Context(), sampleReport, time.Now()); err != nil { // known, with no stream
		t.Fatal(err)
	}
	code, c, msg := enqueue(t, h, "host-1", "", archiveA1)
	if code != http.StatusAccepted {
		t.Fatalf("enqueue: %d %s", code, msg)
	}
	srv := serve(t, h)
	first, _, err := dialStream(t, srv, "host-1", key)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.command(); got.ID != c.ID {
		t.Fatalf("on connect: %+v", got)
	}
	first.conn.CloseNow() // dropped before a result
	second, _, err := dialStream(t, srv, "host-1", key)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.command(); got.ID != c.ID {
		t.Fatalf("on reconnect: %+v", got)
	}
	if _, now := showCmd(t, h, "host-1", c.ID, ""); now.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", now.Attempts)
	}
}

func TestShowCommandWaitIsBounded(t *testing.T) {
	h, _ := cmdServer(t)
	_, c, _ := enqueue(t, h, "host-1", "", archiveA1)
	if code, _ := showCmd(t, h, "host-1", c.ID, "?wait=2h"); code != http.StatusBadRequest {
		t.Errorf("wait 2h: %d, want 400", code)
	}
	start := time.Now()
	if code, got := showCmd(t, h, "host-1", c.ID, "?wait=200ms"); code != http.StatusOK || got.Finished() {
		t.Errorf("wait on an unfinished command: %d %+v", code, got)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("returned before the wait was up")
	}
}

// TestAHostRunsTheServersCommands is the whole path: a host (hostclient.Client, on a fake Paseo
// daemon) connects and reports, the server queues a command, the host receives and runs it, and
// the result lands in Postgres.
func TestAHostRunsTheServersCommands(t *testing.T) {
	d := dbtest.New(t)
	h := instance(t, d, d.Owner)
	srv := serve(t, h)
	daemon := paseotest.New(t, "tok")
	daemon.SetWorkspaces(paseotest.Workspace("w-app", "worktree", "running"))
	ctx, cancel := context.WithCancel(context.Background())
	host := &hostclient.Client{Name: "host-1", Version: "v", Settle: 10 * time.Millisecond, Logf: t.Logf,
		Watcher: &hostclient.Watcher{RetryEvery: 20 * time.Millisecond, Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: daemon.URL(), Token: "tok", ClientID: "test"})
		}},
		Link: &hostclient.Link{Server: srv.URL, Token: keyOf(t, h, "host-1"), Host: "host-1", RetryEvery: 20 * time.Millisecond}}
	stopped := make(chan struct{})
	go func() { host.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if bs := hosts(t, d); len(bs) == 1 && bs[0].Online && bs[0].Report.Paseo.Connected && len(bs[0].Workspaces) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host never reported its daemon: %+v", hosts(t, d))
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, c, _ := enqueue(t, h, "host-1", "", `{"kind":"create_agent","args":{"workspaceId":"w-app","provider":"claude","prompt":"fix it"}}`)
	_, done := showCmd(t, h, "host-1", c.ID, "?wait=10s")
	var res hostclient.CommandResult
	json.Unmarshal(done.Result, &res)
	if done.Status != hostclient.CommandSucceeded || res.AgentID == "" || res.WorkspaceID != "w-app" {
		t.Fatalf("command = %+v", done)
	}
	req := daemon.Requests("agent.create.request")[0]
	if req["initialPrompt"] != "fix it" || req["idempotencyKey"] != c.ID {
		t.Errorf("daemon got %v", req)
	}
	var status string
	_, res2 := sqlPost(t, h, `{"query": "SELECT status FROM commands WHERE id = '`+c.ID+`'"}`)
	if len(res2.Rows) == 1 {
		status, _ = res2.Rows[0][0].(string)
	}
	if status != hostclient.CommandSucceeded {
		t.Errorf("the account's SQL sees %q (%+v)", status, res2)
	}
}
