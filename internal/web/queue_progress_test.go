package web

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// peakHistory records one succeeded run of repo for each peak (one agent each, at that peak) and
// returns the fixed workflow for repo. It leaves the first run's start command for b to drain.
func peakHistory(t *testing.T, h http.Handler, d dbtest.DB, repo string, peaks []int64) string {
	t.Helper()
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: repo}
	wf := createWorkflow(t, h, def)
	first := startRun(t, h, wf, "past job")
	for i, peak := range peaks {
		id := first
		if i > 0 {
			if err := d.Owner.QueryRow(t.Context(), `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status,
				created_at, updated_at, repo, project_selected)
				SELECT account_id, workflow_id, version, host, title, input, 'succeeded', created_at, updated_at, repo, true
				FROM workflow_runs WHERE id=$1 RETURNING id::text`, first).Scan(&id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded', created_at=now()-interval '2 hours',
			finished_at=now()-interval '1 hour' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		agent := fmt.Sprintf("past-%s-%d", repo, i)
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
			VALUES ($1, 'host-1', $2, '{}', '{}', now())`, id, agent); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host, agent_id, minute, mem_max)
			VALUES ('host-1', $1, date_trunc('minute',now()-interval '90 minutes'), $2)`, agent, peak); err != nil {
			t.Fatal(err)
		}
	}
	return wf
}

// One unusually heavy run must not make every later run of its repository reserve its peak, or
// a nearly idle host holds its queue behind estimates its runs never reach. The estimate is what nine runs in ten stayed under.
func TestOneHeavyRunDoesNotSizeEveryRunOfItsRepository(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	peaks := []int64{6 << 30}
	for range 19 {
		peaks = append(peaks, 256<<20)
	}
	wf := peakHistory(t, h, d, "/w/busy", peaks)
	b.command()
	// 9 GiB of 16 used: a 7.5 GiB estimate would pass 90%; a small one fits.
	repositoryMinute(t, b, 7<<30)
	id := startRun(t, h, wf, "answer an email")
	if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(1<<30) {
		t.Fatalf("estimate = %v, want the 1 GiB floor (most runs peak at 256 MiB)", got)
	}
	if got := runField(t, h, id, "status"); got != "starting" {
		t.Fatalf("status = %v, want starting: %v", got, runField(t, h, id, "queue_reason"))
	}
	// Until a repository has ten runs, its heaviest still decides.
	few := peakHistory(t, h, d, "/w/few", []int64{6 << 30, 256 << 20, 256 << 20})
	b.command()
	if got := runField(t, h, startRun(t, h, few, "few runs"), "estimated_memory_bytes"); got != float64(7.5*(1<<30)) {
		t.Fatalf("few-run estimate = %v, want the heaviest plus 25%%", got)
	}
}

// A run waiting for Zach's reply can wait for days with its agents idle. It must not hold its
// memory estimate against the queue meanwhile.
func TestAWaitingRunHoldsNoMemoryForTheQueue(t *testing.T) {
	h, _ := dbServer(t)
	b := sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h) // no history: 4 GiB estimate
	repositoryMinute(t, b, 12<<30)
	waiting := startRun(t, h, wf, "asks Zach")
	succeed(t, b, b.command(), `{"workspaceId":"w-ask","agentId":"orchestrator-ask"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), waiting, map[string]string{"move": "ask", "note": "Which one?"}); code != http.StatusOK {
		t.Fatalf("ask: %d %s", code, msg)
	}
	if got := runField(t, h, waiting, "status"); got != "waiting" {
		t.Fatalf("status = %v", got)
	}
	// 7 GiB of 16 used (44%) and 4 GiB for the new run: 69%. Charging the waiting run's 4 GiB
	// as well would make it 94%, past the 90% limit.
	repositoryMinute(t, b, 9<<30)
	next := startRun(t, h, wf, "next task")
	if got := runField(t, h, next, "status"); got != "starting" {
		t.Fatalf("status = %v, want starting: %v", got, runField(t, h, next, "queue_reason"))
	}
}

// Start now starts a run whatever its host's load. A run that finds its project first must not
// then fall back behind the queue it was started ahead of.
func TestStartNowCarriesThroughProjectSelection(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	repositoryHistory(t, h, d, "/w/app")
	b.command()
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute), 97, 0.5)
	older := startRun(t, h, fixedWorkflow(t, h), "older queued build")
	newer := startRun(t, h, createWorkflow(t, h, workflow.Starting), "newer matched build")
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+newer+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	if got := runField(t, h, newer, "started_by_hand"); got != true {
		t.Fatalf("started_by_hand = %v", got)
	}
	succeed(t, b, b.command(), `{"agentId":"finder"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), newer, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, msg)
	}
	if got := runField(t, h, newer, "status"); got != "starting" {
		t.Fatalf("started by hand, then queued again behind %s: %v (%v)", older, got, runField(t, h, newer, "queue_reason"))
	}
	if got := runField(t, h, older, "status"); got != "queued" {
		t.Fatalf("older run = %v, want still queued", got)
	}
}

// A run started by hand whose start fails retries as soon as its host is ready, not after the
// queue it was started ahead of: only a free queue head would otherwise let it through.
func TestARetryOfAHandStartedRunSkipsTheQueue(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour; c.Now = clock.Now })
	b := sendReport(t, h, wfReport())
	hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0.5)
	wf := fixedWorkflow(t, h)
	older := startRun(t, h, wf, "older queued build")
	newer := startRun(t, h, wf, "started by hand")
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+newer+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	cmd := b.command()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	clock.Add(16 * time.Second)
	hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0.5)
	if err := workflow.NewEngine(d.Owner, clock.Now).RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, newer, "start_retries"); got != float64(1) {
		t.Fatalf("retry of a hand-started run waited in the queue: retries=%v queued=%v reason=%v",
			got, runField(t, h, newer, "start_retry_queued"), runField(t, h, newer, "queue_reason"))
	}
	if got := runField(t, h, older, "status"); got != "queued" {
		t.Fatalf("older run = %v, want still queued", got)
	}
}
