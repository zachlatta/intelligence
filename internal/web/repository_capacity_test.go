package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Completed history has two concurrent workers; the repo needs their sum, not the largest worker.
func repositoryHistory(t *testing.T, h http.Handler, d dbtest.DB, repo string) string {
	t.Helper()
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: repo}
	wf := createWorkflow(t, h, def)
	id := startRun(t, h, wf, "past job")
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded', created_at=now()-interval '2 hours',
		finished_at=now()-interval '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"worker-a", "worker-b"} {
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
			VALUES ($1, 'host-1', $2, '{}', '{}', now())`, id, agent); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host, agent_id, minute, mem_max)
			VALUES ('host-1', $1, date_trunc('minute',now()-interval '90 minutes'), $2)`, agent, int64(4<<30)); err != nil {
			t.Fatal(err)
		}
	}
	return wf
}

func repositoryMinute(t *testing.T, b *rawHost, available uint64) {
	t.Helper()
	cpu := 10.0
	rep := resources.Report{Minutes: []resources.Minute{{Minute: time.Now().UTC().Truncate(time.Minute), Samples: 12,
		CPUs: 8, CPUAvgPct: &cpu, Load1: 1, MemTotal: 16 << 30, MemAvailableMin: available}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

func TestRepositoryEstimateGatesExistingQueue(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	heavy := repositoryHistory(t, h, d, "/w/app")
	b.command()                // historical workspace
	otherRunActive(t, h, d, b) // the head may not start alone
	repositoryMinute(t, b, 7<<30)
	id := startRun(t, h, heavy, "large build")
	if got := runField(t, h, id, "status"); got != "queued" {
		t.Fatalf("heavy repo = %v, want queued", got)
	}
	if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(10<<30) {
		t.Fatalf("concurrent peak plus headroom = %v", got)
	}
	if page := get(t, h, "/workflow-runs/"+id, nil).Body.String(); !strings.Contains(page, "Estimated run memory: 10.0 GiB") {
		t.Fatal("page hides repository estimate")
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+id+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/light"}
	light := createWorkflow(t, h, def)
	first := startRun(t, h, light, "small task")
	second := startRun(t, h, light, "another small task")
	if got := runField(t, h, first, "status"); got != "starting" {
		t.Fatalf("fallback repo = %v", got)
	}
	if got := runField(t, h, second, "status"); got != "queued" {
		t.Fatalf("reservation lost: %v", got)
	}
}

func TestRepositoryMemoryEstimateHoldsStartRetry(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/app")
	b.command()
	otherRunActive(t, h, d, b) // the head may not start alone
	repositoryMinute(t, b, 16<<30)
	id := startRun(t, h, wf, "retry large build")
	cmd := b.command()
	repositoryMinute(t, b, 7<<30)
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "temporary failure"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	clock.Add(16 * time.Second)
	if err := workflow.NewEngine(d.Owner, clock.Now).RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, id, "start_retry_queued"); got != true {
		t.Fatalf("retry did not join queue: %v", got)
	}
	if got := runField(t, h, id, "start_retries"); got != float64(0) {
		t.Fatalf("memory gate spent retry: %v", got)
	}
	repositoryMinute(t, b, 16<<30)
	if got := runField(t, h, id, "start_retries"); got != float64(1) {
		t.Fatalf("retry not admitted: %v", got)
	}
}

func TestMatchedRepositoryReentersExistingQueue(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	repositoryHistory(t, h, d, "/w/app")
	b.command()
	otherRunActive(t, h, d, b) // the head may not start alone
	id := startRun(t, h, createWorkflow(t, h, workflow.Starting), "choose project")
	succeed(t, b, b.command(), `{"agentId":"finder"}`)
	repositoryMinute(t, b, 7<<30)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), id, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("project: %d %s", code, msg)
	}
	if got := runField(t, h, id, "status"); got != "queued" {
		t.Fatalf("selected heavy repo = %v, want queued", got)
	}
	repositoryMinute(t, b, 16<<30)
	if got := runField(t, h, id, "status"); got != "starting" {
		t.Fatalf("queue did not resume selected project: %v", got)
	}
}

func TestOversizedRepositoryRunsAloneWithoutBlockingFIFOForever(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/app")
	b.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(7<<30)); err != nil {
		t.Fatal(err)
	}
	// Queue both while memory is busy, then give the idle host a chance to drain.
	repositoryMinute(t, b, 1<<30)
	heavy := startRun(t, h, wf, "oversized build")
	desk := workflow.Starting
	desk.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/desk"}
	light := startRun(t, h, createWorkflow(t, h, desk), "next job")
	repositoryMinute(t, b, 16<<30)
	if got := runField(t, h, heavy, "status"); got != "starting" {
		t.Fatalf("oversized head never starts: %v", got)
	}
	if got := runField(t, h, light, "status"); got != "queued" {
		t.Fatalf("oversized run must run alone: %v", got)
	}
	// Completion releases the estimate, so the next FIFO entry starts automatically.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded',finished_at=now() WHERE id=$1`, heavy); err != nil {
		t.Fatal(err)
	}
	repositoryMinute(t, b, 16<<30)
	if got := runField(t, h, light, "status"); got != "starting" {
		t.Fatalf("FIFO did not progress: %v", got)
	}
	// An OS baseline can make an estimate never fit beside it, even one smaller than
	// physical RAM. On an idle host the head still starts, so the queue cannot wait for ever.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded',finished_at=now() WHERE id=$1`, light); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(4<<30)); err != nil {
		t.Fatal(err)
	}
	repositoryMinute(t, b, 7<<30)
	baseline := startRun(t, h, wf, "build alongside OS baseline")
	if got := runField(t, h, baseline, "status"); got != "starting" {
		t.Fatalf("idle host kept its head queued behind an OS baseline: %v", got)
	}
}

func TestActiveRepositoryRunKeepsUnconsumedMemoryReservation(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/app")
	b.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(5<<30)); err != nil {
		t.Fatal(err)
	}
	repositoryMinute(t, b, 16<<30)
	first := startRun(t, h, wf, "first heavy run")
	succeed(t, b, b.command(), `{"workspaceId":"heavy-workspace","agentId":"heavy-agent"}`)
	clock.Add(2 * time.Minute)
	minute := clock.Now().UTC().Truncate(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_agents (run_id,host,agent_id,agent,workspace,captured_at)
		VALUES ($1,'host-1','heavy-agent','{}','{}',now());`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host,agent_id,minute,mem_max) VALUES ('host-1','heavy-agent',$1,$2)`, minute, int64(1<<30)); err != nil {
		t.Fatal(err)
	}
	cpu := 10.0
	rep := resources.Report{Minutes: []resources.Minute{{Minute: minute, Samples: 12, CPUs: 8, CPUAvgPct: &cpu, Load1: 1, MemTotal: 16 << 30, MemAvailableMin: 15 << 30}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	second := startRun(t, h, wf, "second heavy run")
	if got := runField(t, h, second, "status"); got != "queued" {
		t.Fatalf("active run lost peak reservation: %v", got)
	}
	reason := runField(t, h, second, "queue_reason").(string)
	if !strings.Contains(reason, "12.5 GiB") || !strings.Contains(reason, "/w/app") || !strings.Contains(reason, "free") {
		t.Fatalf("reason hides estimate or free memory: %s", reason)
	}
	// Use most of the estimate, then show a small job still fits: resident memory
	// must be subtracted from the reservation, not charged a second time.
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+second+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1 WHERE agent_id='heavy-agent'`, int64(10<<30)); err != nil {
		t.Fatal(err)
	}
	rep.Minutes[0].MemAvailableMin = 6 << 30
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// 1 GiB estimate leaves room under the 90% limit alongside 12.5 GiB.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1 WHERE agent_id IN ('worker-a','worker-b')`, int64(128<<20)); err != nil {
		t.Fatal(err)
	}
	small := startRun(t, h, wf, "small learned footprint")
	if got := runField(t, h, small, "status"); got != "starting" {
		t.Fatalf("resident memory charged twice: %v", got)
	}
}
