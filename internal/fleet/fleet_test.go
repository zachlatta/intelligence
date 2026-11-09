package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

var ctx = context.Background()

var t1 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func full() hostclient.Report {
	return hostclient.Report{Name: "host-1", Version: "c1", CollectedAt: t1,
		Paseo: hostclient.PaseoStatus{Connected: true, StatusError: "slow", Version: "0.15.3", Hostname: "host-1",
			Listen: "0.0.0.0:6767", PID: 3029, StartedAt: t1.Add(-5 * time.Hour),
			Providers:   []hostclient.Provider{{Name: "claude", Available: true}, {Name: "pi", Error: "not installed"}},
			ProvidersAt: t1.Add(-time.Minute),
			Workspaces: []hostclient.Workspace{
				{ID: "w1", Name: "login fix", Kind: "worktree", Status: "running", Directory: "/w/w1", Project: "example/app",
					Branch: "fix-login", Dirty: true, Additions: 12, Deletions: 3, CreatedAt: t1.Add(-2 * time.Hour),
					ActivityAt: t1.Add(-time.Minute), LastUserMessageAt: t1.Add(-30 * time.Minute)},
				{ID: "w2", Name: "docs", Kind: "directory", Status: "done", Directory: "/w/docs", CreatedAt: t1.Add(-48 * time.Hour)},
				{ID: "w3", Name: "empty", Kind: "local_checkout", Status: "done", Directory: "/w/app", CreatedAt: t1.Add(-time.Hour)},
			},
			Agents: []hostclient.Agent{
				{ID: "a1", WorkspaceID: "w1", Title: "Fix it", Provider: "claude", Model: "claude-opus-5-5", Thinking: "medium",
					Status: "running", Cwd: "/w/w1", CreatedAt: t1.Add(-time.Hour), UpdatedAt: t1, LastUserMessageAt: t1.Add(-time.Minute)},
				{ID: "a2", WorkspaceID: "w1", ParentID: "a1", Title: "Helper", Provider: "codex", Model: "gpt-6.1-sol",
					Status: "idle", RequiresAttention: true, AttentionReason: "finished", CreatedAt: t1.Add(-50 * time.Minute)},
				{ID: "a3", WorkspaceID: "w2", Title: "Write docs", Provider: "claude", Status: "idle", CreatedAt: t1.Add(-3 * time.Hour)},
			}}}
}

func record(t *testing.T, r *Registry, rep hostclient.Report, at time.Time) {
	t.Helper()
	if err := r.Record(ctx, rep, at); err != nil {
		t.Fatal(err)
	}
}

func list(t *testing.T, r *Registry) []Host {
	t.Helper()
	hosts, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return hosts
}

func TestHostWorkspacesAndAgentsRoundTrip(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	want := full()
	record(t, r, want, time.Now())
	attach(t, r, "host-1", time.Now())
	hosts := list(t, r)
	if len(hosts) != 1 || !hosts[0].Online {
		t.Fatalf("hosts = %+v", hosts)
	}
	b := hosts[0]
	p, wp := b.Report.Paseo, want.Paseo
	if b.Report.Name != "host-1" || b.Report.Version != "c1" || !b.Report.CollectedAt.Equal(t1) || !p.Connected ||
		p.StatusError != "slow" || p.Version != wp.Version || p.Hostname != wp.Hostname || p.Listen != wp.Listen ||
		p.PID != wp.PID || !p.StartedAt.Equal(wp.StartedAt) || !p.ProvidersAt.Equal(wp.ProvidersAt) ||
		len(p.Providers) != 2 || p.Providers[1] != wp.Providers[1] {
		t.Errorf("host = %+v", b.Report)
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
	rep.Paseo.Agents = []hostclient.Agent{
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
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "a9", WorkspaceID: "gone", Status: "closed", Cwd: "/w/gone"})
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

func TestEachReportReplacesTheHostsWorkspacesAndAgents(t *testing.T) {
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

func TestADisconnectedHostShowsNoStaleWork(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	record(t, r, hostclient.Report{Name: "host-1", Paseo: hostclient.PaseoStatus{Error: "connect: refused"}}, time.Now())
	b := list(t, r)[0]
	if b.Report.Paseo.Connected || b.Report.Paseo.Error != "connect: refused" || len(b.Workspaces) != 0 {
		t.Fatalf("host = %+v workspaces %d", b.Report.Paseo, len(b.Workspaces))
	}
}

func TestHostsListByNameAndAreOnlineWhileStreaming(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	now := time.Now()
	record(t, r, hostclient.Report{Name: "zeta"}, now) // reported, but no stream
	record(t, r, hostclient.Report{Name: "alpha"}, now)
	attach(t, r, "alpha", now)
	hosts := list(t, r)
	if len(hosts) != 2 || hosts[0].Report.Name != "alpha" || hosts[1].Report.Name != "zeta" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if !hosts[0].Online || hosts[1].Online {
		t.Errorf("alpha online %v (want true), zeta online %v (want false)", hosts[0].Online, hosts[1].Online)
	}
}

// attach gives host a stream, seen at at.
func attach(t *testing.T, r *Registry, host string, at time.Time) {
	t.Helper()
	if err := r.Attach(ctx, host, "99999999-9999-4999-8999-999999999999", at); err != nil {
		t.Fatal(err)
	}
}

func TestStatusViewsCountWork(t *testing.T) {
	d := dbtest.New(t)
	record(t, New(d.Owner), full(), time.Now())
	attach(t, New(d.Owner), "host-1", time.Now())
	var online bool
	var nw, na, nr int
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, "SELECT online, workspaces, agents, running_agents FROM host_status").Scan(&online, &nw, &na, &nr); err != nil {
		t.Fatal(err)
	}
	if !online || nw != 3 || na != 3 || nr != 1 {
		t.Errorf("host_status: online %v workspaces %d agents %d running %d", online, nw, na, nr)
	}
	var wa, wr int
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, "SELECT agents, running_agents FROM workspace_status WHERE id = 'w1'").Scan(&wa, &wr); err != nil {
		t.Fatal(err)
	}
	if wa != 2 || wr != 1 {
		t.Errorf("workspace_status w1: agents %d running %d", wa, wr)
	}
}

func TestDeletingAHostDeletesItsWorkspacesAndAgents(t *testing.T) {
	d := dbtest.New(t)
	record(t, New(d.Owner), full(), time.Now())
	if _, err := d.Owner.Exec(ctx, "DELETE FROM hosts"); err != nil {
		t.Fatal(err)
	}
	var n int
	d.Owner.QueryRow(ctx, "SELECT (SELECT count(*) FROM workspaces) + (SELECT count(*) FROM agents)").Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left after their host was deleted", n)
	}
}

func TestWorkspacesKeepTheirProjectRoot(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	rep := full()
	rep.Paseo.Workspaces[0].ProjectRoot = "/w/app"
	record(t, r, rep, t1)
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list[0].Workspaces {
		if w.ID == "w1" && w.ProjectRoot != "/w/app" {
			t.Errorf("w1 project root = %q", w.ProjectRoot)
		}
	}
}

func TestWorkspacesKeepTheirTags(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	rep := full()
	rep.Paseo.Workspaces[0].Tags = []string{hostclient.TagPaseoGUI}
	record(t, r, rep, t1)
	for _, w := range list(t, r)[0].Workspaces {
		if want := map[string]int{"w1": 1}[w.ID]; len(w.Tags) != want {
			t.Errorf("%s tags = %v", w.ID, w.Tags)
		}
	}
	// readable with SQL: a workspace with none has an empty array, not null
	var gui []string
	rows, err := dbtest.Reader(t, d, "host-1").Query(ctx,
		"SELECT id FROM workspace_status WHERE 'Paseo GUI' = ANY (tags) AND tags IS NOT NULL ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		gui = append(gui, id)
	}
	if rows.Err() != nil || strings.Join(gui, ",") != "w1" {
		t.Errorf("Paseo GUI workspaces = %v, %v", gui, rows.Err())
	}
	var untagged int
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, "SELECT count(*) FROM workspaces WHERE tags = '{}'").Scan(&untagged); err != nil || untagged != 2 {
		t.Errorf("untagged = %d, %v", untagged, err)
	}
}
