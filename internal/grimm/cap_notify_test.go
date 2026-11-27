package grimm

import (
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestCapChangeNotifiesLiveManager(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id := r.ready() // live manager is agent "mgr", default cap 20

	// Updating the cap notifies the live manager via send_message and records a thread update.
	cap7 := 7
	if err := r.s.UpdateSession(ctx, r.acct, id, SessionUpdate{MaxWorkers: &cap7}); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions WHERE id = $1`, id); got != int32(7) {
		t.Fatalf("max_workers = %v, want 7", got)
	}
	msg := r.take(hostclient.SendMessageKind)
	if msg.Args["agentId"] != "mgr" || !strings.Contains(msg.Args["text"].(string), "7") {
		t.Fatalf("cap notification command = %+v", msg.Args)
	}
	if got := r.one(`SELECT count(*) FROM grimm_messages WHERE session_id = $1 AND kind = 'update' AND body LIKE '%7%'`, id); got != int64(1) {
		t.Errorf("thread update rows = %v, want 1", got)
	}

	// Setting the same cap again does not spam the manager.
	if err := r.s.UpdateSession(ctx, r.acct, id, SessionUpdate{MaxWorkers: &cap7}); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT count(*) FROM commands WHERE kind = 'send_message' AND args->>'agentId' = 'mgr'`); got != int64(1) {
		t.Errorf("send_message count after no-op cap update = %v, want 1", got)
	}

	// /cap command also notifies the live manager.
	if _, err := r.s.Post(ctx, r.acct, id, "/cap 12"); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions WHERE id = $1`, id); got != int32(12) {
		t.Fatalf("max_workers after /cap = %v, want 12", got)
	}
	msg2 := r.take(hostclient.SendMessageKind)
	if msg2.Args["agentId"] != "mgr" || !strings.Contains(msg2.Args["text"].(string), "12") {
		t.Fatalf("/cap notification command = %+v", msg2.Args)
	}
}

func TestCapChangeWhileManagerStartingQueuesWakeup(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		t.Fatal(err)
	}
	r.succeed(r.take(hostclient.CreateWorkspaceKind), `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	if _, err := r.s.Post(ctx, r.acct, id, "Start working"); err != nil {
		t.Fatal(err)
	}
	_ = r.take(hostclient.CreateAgentKind) // manager is still starting (no agent_id yet)

	cap5 := 5
	if err := r.s.UpdateSession(ctx, r.acct, id, SessionUpdate{MaxWorkers: &cap5}); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND delivered_at IS NULL AND detail LIKE '%5%'`, id); got != int64(1) {
		t.Fatalf("pending wakeups for starting manager = %v, want 1", got)
	}
}
