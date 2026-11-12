package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/workflow"
)

const gib = int64(1 << 30)

// namedWorkflow makes a workflow called name, fixed on repo.
func namedWorkflow(t *testing.T, h http.Handler, name, repo string) string {
	t.Helper()
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: repo}
	rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, name, def), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("created = %s", rec.Body)
	}
	return out.ID
}

// pastRuns records one succeeded run of wf for each peak (one agent each, at that peak), as
// history for its memory estimate.
func pastRuns(t *testing.T, d dbtest.DB, wf string, peaks ...int64) {
	t.Helper()
	for i, peak := range peaks {
		var id string
		if err := d.Owner.QueryRow(t.Context(), `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status,
			created_at, updated_at, finished_at, repo, project_selected)
			SELECT w.account_id, w.id, w.version, 'host-1', 'past job', 'past job', 'succeeded', now()-interval '2 hours', now(),
				now()-interval '1 hour', v.definition->'project'->>'directory', true
			FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 RETURNING id::text`, wf).Scan(&id); err != nil {
			t.Fatal(err)
		}
		agent := fmt.Sprintf("past-%s-%d", wf, i)
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
			VALUES ($1, 'host-1', $2, '{}', '{}', now())`, id, agent); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host, agent_id, minute, mem_max)
			VALUES ('host-1', $1, date_trunc('minute',now()-interval '90 minutes'), $2)`, agent, peak); err != nil {
			t.Fatal(err)
		}
	}
}

// memMinute sends host-1's minute at m: total memory, and the least available in it.
func memMinute(t *testing.T, b *rawHost, m time.Time, total, available int64) {
	t.Helper()
	cpu := 8.0
	rep := resources.Report{Minutes: []resources.Minute{{Minute: m, Samples: 12, CPUs: 8, CPUAvgPct: &cpu, Load1: 0.64,
		MemTotal: uint64(total), MemAvailableMin: uint64(available)}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

// askingRun starts a run of wf, starts its orchestrator (agent) and has it ask Zach.
func askingRun(t *testing.T, h http.Handler, b *rawHost, wf, agent string) string {
	t.Helper()
	id := startRun(t, h, wf, "asks Zach")
	succeed(t, b, b.command(), `{"workspaceId":"w-`+agent+`","agentId":"`+agent+`"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), id, map[string]string{"move": "ask", "note": "Sign off?"}); code != http.StatusOK {
		t.Fatalf("ask: %d %s", code, msg)
	}
	return id
}

// A run's memory estimate comes from its own workflow's history in that repository: an email
// answer in desk is small even when Factory Task runs in desk are heavy. A workflow with no
// history of its own falls back to the repository's.
func TestMemoryEstimatesComeFromTheWorkflowsOwnHistory(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	factory := namedWorkflow(t, h, "Factory Task", "/w/desk")
	email := namedWorkflow(t, h, "Zach Email Task", "/w/desk")
	voice := namedWorkflow(t, h, "Voice Memo Received", "/w/desk")
	pastRuns(t, d, factory, 5980<<20) // 5.84 GiB: estimated at 7.3 GiB
	small := []int64{}
	for range 12 {
		small = append(small, 300<<20)
	}
	pastRuns(t, d, email, small...)
	memMinute(t, sendReport(t, h, wfReport()), time.Now().UTC().Truncate(time.Minute), 100*gib, 90*gib)

	check := func(wf string, want int64) {
		t.Helper()
		id := startRun(t, h, wf, "a task")
		if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(want) {
			t.Fatalf("estimate = %.2f GiB, want %.2f GiB", got.(float64)/float64(gib), float64(want)/float64(gib))
		}
	}
	check(email, gib)                   // its own runs peak at 300 MiB: the 1 GiB floor
	check(factory, int64(5980<<20)*5/4) // 7.3 GiB
	_, r := sqlPost(t, h, q(`SELECT estimated_memory_bytes FROM workflow_repository_memory WHERE repo = '/w/desk'`))
	check(voice, int64(r.Rows[0][0].(float64))) // no history of its own: the repository's
	code, r := sqlPost(t, h, q(`SELECT w.name, e.observed_runs, e.estimated_memory_bytes FROM workflow_memory_estimate e
		JOIN workflows w ON w.id = e.workflow_id WHERE e.repo = '/w/desk' ORDER BY w.name`))
	if code != http.StatusOK || len(r.Rows) != 2 || r.Rows[0][0] != "Factory Task" || r.Rows[1][1] != float64(12) {
		t.Fatalf("workflow_memory_estimate: %d %+v", code, r)
	}
}

// The workflow host on 2026-11-11: a 19 GiB host with 12 GiB free, two runs waiting for Zach's
// sign-off whose estimates (7.1 and 3.4 GiB) were reserved in full, one running, and an email
// answer in desk estimated at the 7.3 GiB of desk's Factory Task runs. Fourteen runs sat queued for
// up to 14.5 hours behind it. The head must start.
func TestTheNovember11QueueStarts(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	factory := namedWorkflow(t, h, "Factory Task", "/w/desk")
	email := namedWorkflow(t, h, "Zach Email Task", "/w/desk")
	pastRuns(t, d, factory, 5980<<20)
	minute := time.Now().UTC().Truncate(time.Minute)
	memMinute(t, b, minute.Add(-time.Minute), 19*gib, 17*gib)
	signoff1 := askingRun(t, h, b, factory, "orch-1")
	signoff2 := askingRun(t, h, b, factory, "orch-2")
	running := startRun(t, h, factory, "running")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-run"}`)
	for id, est := range map[string]float64{signoff1: 7.1, signoff2: 3.4, running: 1.4} {
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET estimated_memory_bytes=$2 WHERE id=$1`, id, int64(est*float64(gib))); err != nil {
			t.Fatal(err)
		}
	}
	// Full: the email answer queues behind it.
	memMinute(t, b, minute.Add(-time.Minute), 19*gib, gib)
	head := startRun(t, h, email, "Re: a two-minute email answer")
	if got := runField(t, h, head, "status"); got != "queued" {
		t.Fatalf("status = %v, want queued while memory is full", got)
	}
	if got := runField(t, h, head, "estimated_memory_bytes"); got != float64(int64(5980<<20)*5/4) {
		t.Fatalf("estimate = %v, want desk's 7.3 GiB (the email workflow has no history yet)", got)
	}
	// 11-11 04:30 ET: 12 GiB of 19 free, the host idle.
	memMinute(t, b, minute, 19*gib, 12*gib)
	if got := runField(t, h, head, "status"); got != "starting" {
		t.Fatalf("head = %v: %v", got, runField(t, h, head, "queue_reason"))
	}
}

// When runs waiting for Zach's sign-off are what fills the host, the queue says so plainly, and
// the Workflows page shows the queue head's reason.
func TestTheQueueSaysWhenSignOffsHoldItsMemory(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	factory := namedWorkflow(t, h, "Factory Task", "/w/desk")
	minute := time.Now().UTC().Truncate(time.Minute)
	memMinute(t, b, minute.Add(-time.Minute), 19*gib, 17*gib)
	askingRun(t, h, b, factory, "orch-1")
	askingRun(t, h, b, factory, "orch-2")
	for agent, bytes := range map[string]int64{"orch-1": 7 * gib, "orch-2": 3*gib + gib/2} {
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host, agent_id, minute, mem_max) VALUES ('host-1', $1, $2, $3)`,
			agent, minute, bytes); err != nil {
			t.Fatal(err)
		}
	}
	memMinute(t, b, minute, 19*gib, gib+gib/2)
	head := startRun(t, h, factory, "next build")
	startRun(t, h, factory, "the build after")
	reason, _ := runField(t, h, head, "queue_reason").(string)
	for _, want := range []string{"2 runs waiting for sign-off hold 10.5 GiB", "waiting for 2 sign-offs to free memory"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("queue_reason %q lacks %q", reason, want)
		}
	}
	page := get(t, h, "/workflows", nil).Body.String()
	for _, want := range []string{`class="banner note queue-head" data-host="host-1"`, `2 runs queued on <span class="mono">host-1</span>`, ">Next build</a>. host-1 is near capacity", "waiting for 2 sign-offs to free memory"} {
		if !strings.Contains(page, want) {
			i := strings.Index(page, `queue-head`)
			t.Fatalf("Workflows page lacks %q: %s", want, page[max(i, 0):min(max(i, 0)+600, len(page))])
		}
	}
}

// A failed start keeps its run's memory reservation while its retry is pending, so the next queued
// run cannot take that room and leave the retry with none. The retry then starts within capacity.
func TestAFailedStartsRetryIsNotOvertakenByTheNextQueuedRun(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour; c.Now = clock.Now })
	b := sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h) // no history: 4 GiB each
	// 9 GiB of 16 free: one run fits (69%), a second does not (94%).
	repositoryMinute(t, b, 9<<30)
	first := startRun(t, h, wf, "first")
	cmd := b.command()
	clock.Add(time.Second) // queue order is by created_at, from this clock
	next := startRun(t, h, wf, "next")
	if got := runField(t, h, next, "status"); got != "queued" {
		t.Fatalf("next = %v, want queued", got)
	}
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	e := workflow.NewEngine(d.Owner, clock.Now)
	if err := e.RetryStarts(t.Context()); err != nil { // reconciles, then starts what fits
		t.Fatal(err)
	}
	if got := runField(t, h, next, "status"); got != "queued" {
		t.Fatalf("next run took the failed start's room: %v", got)
	}
	clock.Add(16 * time.Second)
	repositoryMinute(t, b, 9<<30)
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, first, "start_retries"); got != float64(1) {
		t.Fatalf("retry not sent: %v (%v)", got, runField(t, h, first, "queue_reason"))
	}
	if b.command().ID == cmd.ID {
		t.Fatal("no new start command sent")
	}
	if got := runField(t, h, next, "status"); got != "queued" {
		t.Fatalf("next = %v, want still queued behind the retry's reservation", got)
	}
}

// A start retry that is due must not sit for hours, and must not start beyond its host's capacity.
// Once it is RetryWaitLimit past due and still cannot go, the run fails and says why: the host is
// not ready, or it has no room (with the queue reason).
func TestAStuckStartRetryRetriesOrFailsWithinMinutes(t *testing.T) {
	if workflow.RetryWaitLimit > 10*time.Minute {
		t.Fatalf("RetryWaitLimit = %v, want at most 10 minutes", workflow.RetryWaitLimit)
	}
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "host full", false: "daemon away"}[ready], func(t *testing.T) {
			d := dbtest.New(t)
			clock := &fakeClock{t: time.Now().UTC()}
			h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour; c.Now = clock.Now })
			rep := wfReport()
			b := sendReport(t, h, rep)
			wf := fixedWorkflow(t, h)
			run := startRun(t, h, wf, "queue-started run")
			cmd := b.command()
			// The host fills up, and the run's start fails on a daemon disconnect.
			hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0.5)
			clock.Add(time.Second) // queue order is by created_at, from this clock
			startRun(t, h, wf, "behind it")
			if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			if !ready {
				rep.Paseo.Connected = false
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
			}
			e := workflow.NewEngine(d.Owner, clock.Now)
			for range 4 { // a minute past due: still waiting
				clock.Add(20 * time.Second)
				hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0.5)
				if err := e.RetryStarts(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if got := runField(t, h, run, "start_retries"); got != float64(0) {
				t.Fatalf("retried beyond capacity: %v", got)
			}
			clock.Add(workflow.RetryWaitLimit)
			hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0.5)
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := runField(t, h, run, "start_retries"); got != float64(0) {
				t.Fatalf("overdue retry sent beyond capacity: %v", got)
			}
			r := readRun(t, h, run)
			msg, _ := r["error"].(string)
			want := map[bool][]string{true: {"still had no room", "host-1 is near capacity: CPU 97%"}, false: {"Paseo daemon was still not connected"}}[ready]
			for _, w := range append(want, "was due at", "not connected to the Paseo daemon") {
				if r["status"] != "failed" || !strings.Contains(msg, w) {
					t.Fatalf("status %v, error %q lacks %q", r["status"], msg, w)
				}
			}
		})
	}
}
