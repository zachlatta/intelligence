package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// cmdServer takes commands with the operator token "op". It never polls for commands, so one
// that arrives does so because the enqueue woke the box's stream.
func cmdServer(t *testing.T) (http.Handler, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	h := New(Config{Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret", OperatorToken: "op",
		CommandPoll: time.Hour})
	sendReport(t, h, sampleReport)
	return h, d
}

func enqueue(t *testing.T, h http.Handler, box, token, body string) (int, fleet.Command, string) {
	t.Helper()
	rec := post(t, h, "/api/devboxes/"+box+"/commands", token, []byte(body))
	var c fleet.Command
	if rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if loc := rec.Header().Get("Location"); loc != "/api/devboxes/"+box+"/commands/"+c.ID {
			t.Errorf("location = %q", loc)
		}
	}
	return rec.Code, c, rec.Body.String()
}

func showCmd(t *testing.T, h http.Handler, box, id, query string) (int, fleet.Command) {
	t.Helper()
	rec := get(t, h, "/api/devboxes/"+box+"/commands/"+id+query, nil)
	var c fleet.Command
	if rec.Code == http.StatusOK {
		json.Unmarshal(rec.Body.Bytes(), &c)
	}
	return rec.Code, c
}

const archiveA1 = `{"kind":"archive_agent","args":{"agentId":"a1"}}`

func TestCommandsNeedTheOperatorToken(t *testing.T) {
	h, _ := cmdServer(t)
	for _, tok := range []string{"", "wrong", "secret"} { // the dev box token is not the operator's
		if code, _, _ := enqueue(t, h, "devbox-1", tok, archiveA1); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	nobody, _ := dbServer(t) // no operator token configured
	if code, _, _ := enqueue(t, nobody, "devbox-1", "", archiveA1); code != http.StatusServiceUnavailable {
		t.Errorf("no OPERATOR_TOKEN: %d, want 503", code)
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
		if code, _, msg := enqueue(t, h, "devbox-1", "op", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, msg, want)
		}
	}
	if code, _, _ := enqueue(t, h, "nobody", "op", archiveA1); code != http.StatusNotFound {
		t.Errorf("unknown box: %d, want 404", code)
	}
	if code, _ := showCmd(t, h, "devbox-1", "not-an-id", ""); code != http.StatusNotFound {
		t.Errorf("unknown command: %d", code)
	}
}

func TestACommandGoesDownTheStreamAtOnceAndItsResultComesBack(t *testing.T) {
	h, _ := cmdServer(t)
	b := openStream(t, h, "devbox-1")
	code, c, msg := enqueue(t, h, "devbox-1", "op", `{"kind":"create_agent","args":{"cwd":"/w/app","provider":"claude","prompt":"go"},"ttl":"1m"}`)
	if code != http.StatusAccepted || c.Status != devbox.CommandQueued || !c.ExpiresAt.Equal(c.CreatedAt.Add(time.Minute)) {
		t.Fatalf("enqueue: %d %s", code, msg)
	}
	got := b.command()
	if got.ID != c.ID || got.Kind != devbox.CreateAgentKind || !strings.Contains(string(got.Args), `"prompt":"go"`) {
		t.Fatalf("command = %+v", got)
	}
	if code, other := showCmd(t, h, "devbox-2", c.ID, ""); code != http.StatusNotFound {
		t.Errorf("another box's path: %d %+v", code, other)
	}
	_, sent := showCmd(t, h, "devbox-1", c.ID, "")
	if sent.Status != devbox.CommandSent || sent.Attempts != 1 {
		t.Errorf("after sending: %+v", sent)
	}
	done := make(chan fleet.Command)
	go func() { _, c := showCmd(t, h, "devbox-1", c.ID, "?wait=5s"); done <- c }()
	b.call(devbox.Frame{Type: devbox.CommandUpdateFrame, Update: &devbox.CommandUpdate{ID: c.ID, Status: devbox.CommandRunning}})
	b.call(devbox.Frame{Type: devbox.CommandUpdateFrame, Update: &devbox.CommandUpdate{ID: c.ID, Status: devbox.CommandSucceeded,
		Result: json.RawMessage(`{"agentId":"a9"}`)}})
	select {
	case fin := <-done:
		if fin.Status != devbox.CommandSucceeded || string(fin.Result) != `{"agentId":"a9"}` || fin.StartedAt.IsZero() {
			t.Errorf("finished = %+v", fin)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("?wait never returned")
	}
	if ack := b.call(devbox.Frame{Type: devbox.CommandUpdateFrame, Update: &devbox.CommandUpdate{ID: "00000000-0000-4000-8000-000000000000",
		Status: devbox.CommandFailed}}); ack.Error != "" {
		t.Errorf("an update for an unknown command is dropped, not refused: %+v", ack)
	}
}

func TestQueuedCommandsWaitForTheBoxAndUnfinishedOnesAreResent(t *testing.T) {
	h, _ := cmdServer(t)
	_, c, _ := enqueue(t, h, "devbox-1", "op", archiveA1)
	srv := serve(t, h)
	first, _, err := dialStream(t, srv, "devbox-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := first.command(); got.ID != c.ID {
		t.Fatalf("on connect: %+v", got)
	}
	first.conn.CloseNow() // dropped before a result
	second, _, err := dialStream(t, srv, "devbox-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := second.command(); got.ID != c.ID {
		t.Fatalf("on reconnect: %+v", got)
	}
	if _, now := showCmd(t, h, "devbox-1", c.ID, ""); now.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", now.Attempts)
	}
}

func TestShowCommandWaitIsBounded(t *testing.T) {
	h, _ := cmdServer(t)
	_, c, _ := enqueue(t, h, "devbox-1", "op", archiveA1)
	if code, _ := showCmd(t, h, "devbox-1", c.ID, "?wait=2h"); code != http.StatusBadRequest {
		t.Errorf("wait 2h: %d, want 400", code)
	}
	start := time.Now()
	if code, got := showCmd(t, h, "devbox-1", c.ID, "?wait=200ms"); code != http.StatusOK || got.Status != devbox.CommandQueued {
		t.Errorf("wait on a queued command: %d %+v", code, got)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("returned before the wait was up")
	}
}

// TestADevBoxRunsTheServersCommands is the whole path: the server queues a command, a dev box
// (its Link, Runner and Watcher, on a fake Paseo daemon) receives and runs it, and the result
// lands in Postgres.
func TestADevBoxRunsTheServersCommands(t *testing.T) {
	h, d := cmdServer(t)
	srv := serve(t, h)
	daemon := paseotest.New(t, "tok")
	daemon.SetWorkspaces(paseotest.Workspace("w-app", "worktree", "running"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &devbox.Watcher{RetryEvery: 20 * time.Millisecond, Dial: func(ctx context.Context) (*paseo.Client, error) {
		return paseo.Dial(ctx, paseo.Target{URL: daemon.URL(), Token: "tok", ClientID: "test"})
	}}
	go w.Run(ctx)
	link := &devbox.Link{Server: srv.URL, Token: "secret", Box: "devbox-1", RetryEvery: 20 * time.Millisecond}
	go link.Run(ctx)
	runner := &devbox.Runner{Exec: w.Execute, Report: link.Update, Retry: 20 * time.Millisecond}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-link.Commands():
				runner.Handle(ctx, c)
			}
		}
	}()
	select {
	case <-link.Connected():
	case <-time.After(5 * time.Second):
		t.Fatalf("box never connected: %s", link.Err())
	}
	for !w.Snapshot().Connected {
		time.Sleep(10 * time.Millisecond)
	}
	_, c, _ := enqueue(t, h, "devbox-1", "op", `{"kind":"create_agent","args":{"workspaceId":"w-app","provider":"claude","prompt":"fix it"}}`)
	_, done := showCmd(t, h, "devbox-1", c.ID, "?wait=10s")
	var res devbox.CommandResult
	json.Unmarshal(done.Result, &res)
	if done.Status != devbox.CommandSucceeded || res.AgentID == "" || res.WorkspaceID != "w-app" {
		t.Fatalf("command = %+v", done)
	}
	req := daemon.Requests("agent.create.request")[0]
	if req["initialPrompt"] != "fix it" || req["idempotencyKey"] != c.ID {
		t.Errorf("daemon got %v", req)
	}
	var status string
	if err := d.Readonly.QueryRow(context.Background(), "SELECT status FROM commands WHERE id = $1", c.ID).Scan(&status); err != nil ||
		status != devbox.CommandSucceeded {
		t.Errorf("read-only SQL sees %q (%v)", status, err)
	}
}
