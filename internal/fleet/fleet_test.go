package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/devbox"
)

var ctx = context.Background()

var t1 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func full() devbox.Report {
	return devbox.Report{Name: "devbox-1", Version: "c1", CollectedAt: t1,
		Paseo: devbox.PaseoStatus{Connected: true, StatusError: "slow", Version: "0.15.3", Hostname: "devbox-1",
			Listen: "0.0.0.0:6767", PID: 3029, StartedAt: t1.Add(-5 * time.Hour),
			Providers:   []devbox.Provider{{Name: "claude", Available: true}, {Name: "pi", Error: "not installed"}},
			ProvidersAt: t1.Add(-time.Minute),
			Workspaces: []devbox.Workspace{
				{ID: "w1", Name: "login fix", Kind: "worktree", Status: "running", Directory: "/w/w1", Project: "example/app",
					Branch: "fix-login", Dirty: true, Additions: 12, Deletions: 3, CreatedAt: t1.Add(-2 * time.Hour),
					ActivityAt: t1.Add(-time.Minute), LastUserMessageAt: t1.Add(-30 * time.Minute)},
				{ID: "w2", Name: "docs", Kind: "directory", Status: "done", Directory: "/w/docs", CreatedAt: t1.Add(-48 * time.Hour)},
				{ID: "w3", Name: "empty", Kind: "local_checkout", Status: "done", Directory: "/w/app", CreatedAt: t1.Add(-time.Hour)},
			},
			Agents: []devbox.Agent{
				{ID: "a1", WorkspaceID: "w1", Title: "Fix it", Provider: "claude", Model: "claude-opus-5-5", Thinking: "medium",
					Status: "running", Cwd: "/w/w1", CreatedAt: t1.Add(-time.Hour), UpdatedAt: t1, LastUserMessageAt: t1.Add(-time.Minute)},
				{ID: "a2", WorkspaceID: "w1", ParentID: "a1", Title: "Helper", Provider: "codex", Model: "gpt-6.1-sol",
					Status: "idle", RequiresAttention: true, AttentionReason: "finished", CreatedAt: t1.Add(-50 * time.Minute)},
				{ID: "a3", WorkspaceID: "w2", Title: "Write docs", Provider: "claude", Status: "idle", CreatedAt: t1.Add(-3 * time.Hour)},
			}}}
}

func record(t *testing.T, r *Registry, rep devbox.Report, at time.Time) {
	t.Helper()
	if err := r.Record(ctx, rep, at); err != nil {
		t.Fatal(err)
	}
}

func list(t *testing.T, r *Registry) []Box {
	t.Helper()
	boxes, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return boxes
}

func TestBoxWorkspacesAndAgentsRoundTrip(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	want := full()
	record(t, r, want, time.Now())
	boxes := list(t, r)
	if len(boxes) != 1 || !boxes[0].Online {
		t.Fatalf("boxes = %+v", boxes)
	}
	b := boxes[0]
	p, wp := b.Report.Paseo, want.Paseo
	if b.Report.Name != "devbox-1" || b.Report.Version != "c1" || !b.Report.CollectedAt.Equal(t1) || !p.Connected ||
		p.StatusError != "slow" || p.Version != wp.Version || p.Hostname != wp.Hostname || p.Listen != wp.Listen ||
		p.PID != wp.PID || !p.StartedAt.Equal(wp.StartedAt) || !p.ProvidersAt.Equal(wp.ProvidersAt) ||
		len(p.Providers) != 2 || p.Providers[1] != wp.Providers[1] {
		t.Errorf("box = %+v", b.Report)
	}
	if len(b.Workspaces) != 3 {
		t.Fatalf("workspaces = %+v", b.Workspaces)
	}
	w1 := b.Workspaces[0]
	if w1.ID != "w1" || !w1.Listed || w1.Workspace.Name != "login fix" || w1.Kind != "worktree" || w1.Branch != "fix-login" ||
		!w1.Dirty || w1.Additions != 12 || w1.Deletions != 3 || w1.Project != "example/app" || !w1.ActivityAt.Equal(wp.Workspaces[0].ActivityAt) {
		t.Errorf("w1 = %+v", w1)
	}
	if len(w1.Agents) != 2 || w1.Agents[0].Agent != wp.Agents[0] || w1.Agents[1].Agent != wp.Agents[1] {
		t.Errorf("w1 agents = %+v", w1.Agents)
	}
}

func TestWorkspacesWithWorkFirst(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	var order []string
	for _, w := range list(t, r)[0].Workspaces {
		order = append(order, w.ID)
	}
	// w1 has a running agent; w2 has an idle one; w3 has none
	if strings.Join(order, ",") != "w1,w2,w3" {
		t.Errorf("order = %v, want running agents first, then any agents, then none", order)
	}
}

func TestAgentsAreRunningFirstThenNewest(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	rep := full()
	rep.Paseo.Agents = []devbox.Agent{
		{ID: "old", WorkspaceID: "w1", Status: "idle", CreatedAt: t1.Add(-3 * time.Hour)},
		{ID: "run", WorkspaceID: "w1", Status: "running", CreatedAt: t1.Add(-4 * time.Hour)},
		{ID: "new", WorkspaceID: "w1", Status: "idle", CreatedAt: t1.Add(-time.Hour)},
	}
	record(t, r, rep, time.Now())
	var order []string
	for _, a := range list(t, r)[0].Workspaces[0].Agents {
		order = append(order, a.ID)
	}
	if strings.Join(order, ",") != "run,new,old" {
		t.Errorf("order = %v", order)
	}
}

func TestAnAgentInAnUnlistedWorkspaceIsKept(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	rep := full()
	rep.Paseo.Agents = append(rep.Paseo.Agents, devbox.Agent{ID: "a9", WorkspaceID: "gone", Status: "closed", Cwd: "/w/gone"})
	record(t, r, rep, time.Now())
	var found *WorkspaceAgents
	for _, w := range list(t, r)[0].Workspaces {
		if w.ID == "gone" {
			found = &w
		}
	}
	if found == nil || found.Listed || len(found.Agents) != 1 || found.Agents[0].ID != "a9" {
		t.Fatalf("unlisted workspace = %+v", found)
	}
}

func TestEachReportReplacesTheBoxsWorkspacesAndAgents(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), time.Now())
	rep := full()
	rep.Paseo.Workspaces = rep.Paseo.Workspaces[1:2] // only w2 left
	rep.Paseo.Agents = rep.Paseo.Agents[2:]          // only a3, in w2
	record(t, r, rep, time.Now())
	ws := list(t, r)[0].Workspaces
	if len(ws) != 1 || ws[0].ID != "w2" || len(ws[0].Agents) != 1 || ws[0].Agents[0].ID != "a3" {
		t.Fatalf("workspaces = %+v", ws)
	}
	var nw, na int
	d.Owner.QueryRow(ctx, "SELECT (SELECT count(*) FROM workspaces), (SELECT count(*) FROM agents)").Scan(&nw, &na)
	if nw != 1 || na != 1 {
		t.Errorf("rows: %d workspaces %d agents", nw, na)
	}
}

func TestADisconnectedBoxShowsNoStaleWork(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	record(t, r, devbox.Report{Name: "devbox-1", Paseo: devbox.PaseoStatus{Error: "connect: refused"}}, time.Now())
	b := list(t, r)[0]
	if b.Report.Paseo.Connected || b.Report.Paseo.Error != "connect: refused" || len(b.Workspaces) != 0 {
		t.Fatalf("box = %+v workspaces %d", b.Report.Paseo, len(b.Workspaces))
	}
}

func TestBoxesListByNameAndGoOfflineAfterAMinute(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	now := time.Now()
	record(t, r, devbox.Report{Name: "zeta"}, now.Add(-2*time.Minute))
	record(t, r, devbox.Report{Name: "alpha"}, now)
	boxes := list(t, r)
	if len(boxes) != 2 || boxes[0].Report.Name != "alpha" || boxes[1].Report.Name != "zeta" {
		t.Fatalf("boxes = %+v", boxes)
	}
	if !boxes[0].Online || boxes[1].Online {
		t.Errorf("alpha online %v (want true), zeta online %v (want false)", boxes[0].Online, boxes[1].Online)
	}
}

func TestStatusViewsCountWork(t *testing.T) {
	d := dbtest.New(t)
	record(t, New(d.Owner), full(), time.Now())
	var online bool
	var nw, na, nr int
	if err := d.Readonly.QueryRow(ctx, "SELECT online, workspaces, agents, running_agents FROM devbox_status").Scan(&online, &nw, &na, &nr); err != nil {
		t.Fatal(err)
	}
	if !online || nw != 3 || na != 3 || nr != 1 {
		t.Errorf("devbox_status: online %v workspaces %d agents %d running %d", online, nw, na, nr)
	}
	var wa, wr int
	if err := d.Readonly.QueryRow(ctx, "SELECT agents, running_agents FROM workspace_status WHERE id = 'w1'").Scan(&wa, &wr); err != nil {
		t.Fatal(err)
	}
	if wa != 2 || wr != 1 {
		t.Errorf("workspace_status w1: agents %d running %d", wa, wr)
	}
}

func TestDeletingABoxDeletesItsWorkspacesAndAgents(t *testing.T) {
	d := dbtest.New(t)
	record(t, New(d.Owner), full(), time.Now())
	if _, err := d.Owner.Exec(ctx, "DELETE FROM devboxes"); err != nil {
		t.Fatal(err)
	}
	var n int
	d.Owner.QueryRow(ctx, "SELECT (SELECT count(*) FROM workspaces) + (SELECT count(*) FROM agents)").Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left after their box was deleted", n)
	}
}
