package devbox

import (
	"context"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

const tok = "tok_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func watcher(t *testing.T, d *paseotest.Daemon) *Watcher {
	t.Helper()
	w := &Watcher{
		Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: d.URL(), Token: tok, ClientID: "test"})
		},
		RetryEvery: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	return w
}

// eventually waits for cond on the watcher's snapshot.
func eventually(t *testing.T, w *Watcher, what string, cond func(PaseoStatus) bool) PaseoStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := w.Snapshot()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s; last snapshot %+v", what, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func daemon(t *testing.T) *paseotest.Daemon {
	d := paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"), paseotest.Workspace("w2", "directory", "done"))
	d.SetAgents(paseotest.Agent("a1", "w1", "running"), paseotest.Agent("a2", "w1", "idle"))
	return d
}

func TestWatcherReadsTheDaemon(t *testing.T) {
	w := watcher(t, daemon(t))
	s := eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 2 })
	if !s.Connected || s.Error != "" || s.Version != "0.15.3" || s.Hostname != "devbox-1" || s.PID != 42 ||
		s.Listen != "0.0.0.0:6767" || s.StartedAt.IsZero() || len(s.Providers) != 2 || s.ProvidersAt.IsZero() {
		t.Errorf("daemon fields = %+v", s)
	}
	if p := s.Providers[1]; p.Name != "pi" || p.Available || p.Error != "not installed" {
		t.Errorf("provider = %+v", p)
	}
	if len(s.Workspaces) != 2 {
		t.Fatalf("workspaces = %+v", s.Workspaces)
	}
	ws := s.Workspaces[0]
	if ws.ID != "w1" || ws.Kind != "worktree" || ws.Status != "running" || ws.Branch != "feature/x" || !ws.Dirty ||
		ws.Additions != 12 || ws.Deletions != 3 || ws.Project != "example/app" || ws.Directory != "/w/w1" || ws.CreatedAt.IsZero() {
		t.Errorf("workspace = %+v", ws)
	}
	a := s.Agents[0]
	if a.ID != "a1" || a.WorkspaceID != "w1" || a.Title != "Agent a1" || a.Provider != "claude" || a.Model != "claude-opus-5-5" ||
		a.Thinking != "medium" || a.Status != "running" || a.ParentID != "parent-1" || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Errorf("agent = %+v", a)
	}
}

func TestWatcherAppliesPushedUpdatesAndSignalsThem(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 2 })
	drain(w)
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": paseotest.Agent("a3", "w2", "running")})
	d.Push("agent_update", map[string]any{"kind": "remove", "agentId": "a2"})
	archived := paseotest.Agent("a1", "w1", "idle")
	archived["archivedAt"] = "2026-11-08T12:00:00.000Z"
	d.Push("agent_update", map[string]any{"kind": "upsert", "agent": archived})
	d.Push("workspace_update", map[string]any{"kind": "remove", "id": "w1"})
	s := eventually(t, w, "updates", func(s PaseoStatus) bool {
		return len(s.Agents) == 1 && s.Agents[0].ID == "a3" && len(s.Workspaces) == 1 && s.Workspaces[0].ID == "w2"
	})
	_ = s
	select {
	case <-w.Changed():
	case <-time.After(time.Second):
		t.Errorf("updates were applied without signalling Changed")
	}
}

func TestWatcherReportsADaemonThatIsDown(t *testing.T) {
	w := &Watcher{Dial: func(ctx context.Context) (*paseo.Client, error) {
		return paseo.Dial(ctx, paseo.Target{URL: "ws://127.0.0.1:1/ws", Token: tok, ClientID: "x"})
	}, RetryEvery: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	s := eventually(t, w, "an error", func(s PaseoStatus) bool { return s.Error != "" })
	if s.Connected || s.Agents != nil || s.Workspaces != nil {
		t.Errorf("a down daemon must report no agents or workspaces as current: %+v", s)
	}
}

func TestWatcherReconnectsAfterTheDaemonRestarts(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "agents", func(s PaseoStatus) bool { return len(s.Agents) == 2 })
	d.SetAgents(paseotest.Agent("a9", "w2", "idle"))
	d.DropAll()
	s := eventually(t, w, "the new agent list after reconnecting", func(s PaseoStatus) bool {
		return s.Connected && len(s.Agents) == 1 && s.Agents[0].ID == "a9"
	})
	if s.Error != "" {
		t.Errorf("error %q left after reconnecting", s.Error)
	}
	if d.Connections() < 2 {
		t.Errorf("connections = %d, want a reconnect", d.Connections())
	}
}

func TestStatusFailureKeepsTheLastProviders(t *testing.T) {
	d := daemon(t)
	w := watcher(t, d)
	first := eventually(t, w, "providers", func(s PaseoStatus) bool { return len(s.Providers) == 2 })
	d.FailStatus("status exploded")
	w.RefreshStatus(context.Background())
	s := w.Snapshot()
	if s.StatusError == "" || len(s.Providers) != 2 || !s.ProvidersAt.Equal(first.ProvidersAt) || !s.Connected {
		t.Errorf("snapshot = %+v", s)
	}
	d.FailStatus("")
	w.RefreshStatus(context.Background())
	if s := w.Snapshot(); s.StatusError != "" || !s.ProvidersAt.After(first.ProvidersAt) {
		t.Errorf("a good status read should clear the error and refresh providers: %+v", s)
	}
}

func drain(w *Watcher) {
	for {
		select {
		case <-w.Changed():
		default:
			return
		}
	}
}
