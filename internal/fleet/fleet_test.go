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

func full() devbox.Report {
	return devbox.Report{Name: "devbox-1", Version: "c1", CollectedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Paseo: devbox.PaseoStatus{Daemon: "running", Connected: "reachable", Version: "0.11.4", Hostname: "devbox-1",
			Listen: "0.0.0.0:6767", PID: 3029, StartedAt: time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC), Note: "n",
			Providers:   []devbox.Provider{{Name: "claude", Status: "available", Available: true}, {Name: "pi", Status: "unavailable"}},
			ProvidersAt: time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC), Agents: []devbox.Agent{
				{ID: "id-1", ShortID: "id1", Name: "Fix it", Provider: "claude/claude-opus-5-5", Thinking: "medium", Status: "running", Cwd: "~/dev/x"},
				{ID: "id-2", ShortID: "id2", Name: "Read it", Provider: "codex/gpt-6.1-sol", Thinking: "high", Status: "idle", Cwd: "~/dev/y"}}}}
}

func TestReportRoundTripsThroughPostgres(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	want := full()
	if err := r.Record(ctx, want, time.Now()); err != nil {
		t.Fatal(err)
	}
	boxes, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || !boxes[0].Online {
		t.Fatalf("boxes = %+v", boxes)
	}
	got := boxes[0].Report
	p, wp := got.Paseo, want.Paseo
	if got.Name != want.Name || got.Version != want.Version || !got.CollectedAt.Equal(want.CollectedAt) ||
		p.Daemon != wp.Daemon || p.Connected != wp.Connected || p.Version != wp.Version || p.Hostname != wp.Hostname ||
		p.Listen != wp.Listen || p.PID != wp.PID || !p.StartedAt.Equal(wp.StartedAt) || p.Note != wp.Note ||
		!p.ProvidersAt.Equal(wp.ProvidersAt) || len(p.Providers) != 2 || p.Providers[0] != wp.Providers[0] ||
		p.Providers[1] != wp.Providers[1] || len(p.Agents) != 2 || p.Agents[0] != wp.Agents[0] || p.Agents[1] != wp.Agents[1] {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestErrorsAndMissingAgentsRoundTrip(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	rep := devbox.Report{Name: "broken", Paseo: devbox.PaseoStatus{Error: "paseo status: not found", AgentsError: "paseo ls: boom"}}
	if err := r.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	boxes, _ := r.List(ctx)
	p := boxes[0].Report.Paseo
	if p.Error != rep.Paseo.Error || p.AgentsError != rep.Paseo.AgentsError || p.Agents != nil || !p.StartedAt.IsZero() || !boxes[0].Report.CollectedAt.IsZero() {
		t.Errorf("got %+v", p)
	}
}

func TestBoxesListByNameAndGoOfflineAfterAMinute(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	now := time.Now()
	if err := r.Record(ctx, devbox.Report{Name: "zeta"}, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(ctx, devbox.Report{Name: "alpha"}, now); err != nil {
		t.Fatal(err)
	}
	boxes, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 2 || boxes[0].Report.Name != "alpha" || boxes[1].Report.Name != "zeta" {
		t.Fatalf("boxes = %+v, want alpha then zeta", boxes)
	}
	if !boxes[0].Online || boxes[1].Online {
		t.Errorf("alpha online %v (want true), zeta online %v (want false)", boxes[0].Online, boxes[1].Online)
	}
	if !boxes[1].LastSeen.Equal(now.Add(-2 * time.Minute).Truncate(time.Microsecond)) {
		t.Errorf("zeta last seen %v", boxes[1].LastSeen)
	}
}

func TestLatestReportReplacesEarlier(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	r.Record(ctx, devbox.Report{Name: "box", Paseo: devbox.PaseoStatus{Daemon: "running"}}, time.Now())
	r.Record(ctx, devbox.Report{Name: "box", Paseo: devbox.PaseoStatus{Daemon: "stopped"}}, time.Now())
	boxes, _ := r.List(ctx)
	if len(boxes) != 1 || boxes[0].Report.Paseo.Daemon != "stopped" {
		t.Fatalf("boxes = %+v", boxes)
	}
}

func TestAgentListIsReplacedByEachReport(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	rep := full()
	if err := r.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	rep.Paseo.Agents = rep.Paseo.Agents[1:] // the running one finished and was archived
	if err := r.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	boxes, _ := r.List(ctx)
	if got := boxes[0].Report.Paseo.Agents; len(got) != 1 || got[0].ID != "id-2" {
		t.Fatalf("agents = %+v, want only id-2", got)
	}
	var n int
	if err := d.Owner.QueryRow(ctx, "SELECT count(*) FROM devbox_agents").Scan(&n); err != nil || n != 1 {
		t.Fatalf("devbox_agents rows = %d, err %v", n, err)
	}
}

func TestAFailedAgentListShowsNoStaleAgents(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	rep := full()
	r.Record(ctx, rep, time.Now())
	rep.Paseo.Agents, rep.Paseo.AgentsError = nil, "paseo ls: timeout"
	r.Record(ctx, rep, time.Now())
	boxes, _ := r.List(ctx)
	if p := boxes[0].Report.Paseo; len(p.Agents) != 0 || p.AgentsError == "" {
		t.Fatalf("agents %+v error %q: a failed list must not show the last one as current", p.Agents, p.AgentsError)
	}
}

func TestAgentsAreListedRunningFirstThenByName(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	rep := devbox.Report{Name: "box", Paseo: devbox.PaseoStatus{Agents: []devbox.Agent{
		{ID: "1", Name: "b", Status: "idle"}, {ID: "2", Name: "z", Status: "running"},
		{ID: "3", Name: "a", Status: "idle"}, {ID: "4", Name: "c", Status: "closed"}}}}
	r.Record(ctx, rep, time.Now())
	boxes, _ := r.List(ctx)
	var order []string
	for _, a := range boxes[0].Report.Paseo.Agents {
		order = append(order, a.ID)
	}
	if strings.Join(order, ",") != "2,4,3,1" {
		t.Fatalf("order = %v, want running first, then by status and name (2,4,3,1)", order)
	}
}

func TestDeletingABoxDeletesItsAgents(t *testing.T) {
	d := dbtest.New(t)
	New(d.Owner).Record(ctx, full(), time.Now())
	if _, err := d.Owner.Exec(ctx, "DELETE FROM devboxes"); err != nil {
		t.Fatal(err)
	}
	var n int
	d.Owner.QueryRow(ctx, "SELECT count(*) FROM devbox_agents").Scan(&n)
	if n != 0 {
		t.Fatalf("%d agents left after their box was deleted", n)
	}
}
