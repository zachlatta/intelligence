package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// The workflow host on 2026-11-16: 24 CPUs at 19% and 6.3 of 19.3 GiB in use, yet eight runs sat
// queued for hours. A long Factory Task in a heavy repository (its runs peak near 9 GiB; it used
// 1.2 GiB) and two lighter runs were going. Admission reserved every one of their peaks at once
// (12 GiB) and kept the queue strictly in order, so its head, a second heavy run, held seven small
// runs behind it, each needing 1 GiB to find its project.
//
// A new run needs room for its own peak and the largest peak still to come of the runs going, not
// for all of them: two heavy runs cannot meet on it, but small runs fit beside one. A run that fits starts ahead of a head that the
// runs older than it are holding. Once only the runs that started after the head keep it waiting,
// the queue holds for it, so it is not starved, and it starts when they end.
func TestTheNovember16QueueStartsSmallRunsAroundAHeavyHead(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	heavy := namedWorkflow(t, h, "Factory Task", "/w/pdw")
	pastRuns(t, d, heavy, 9090<<20) // synthetic peak estimated at 8.9 GiB, without a multiplier
	light := namedWorkflow(t, h, "Light Task", "/w/light")
	finder := createWorkflow(t, h, workflow.Starting) // finds its project: 1 GiB
	memMinute(t, b, clock.Now().Add(-time.Minute), 19*gib, 17*gib)

	going := func(wf, agent string, estimate int64) string {
		t.Helper()
		id := startRun(t, h, wf, "going: "+agent)
		if runField(t, h, id, "status") == "queued" {
			if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+id+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
				t.Fatal(rec.Body)
			}
		}
		succeed(t, b, b.command(), `{"workspaceId":"w-`+agent+`","agentId":"`+agent+`"}`)
		if estimate > 0 {
			if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET estimated_memory_bytes=$2 WHERE id=$1`, id, estimate); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	long := going(heavy, "orch-long", 0)
	going(light, "orch-s1", 2100<<20)
	going(light, "orch-s2", 2400<<20)
	resident := func(at time.Time, agents map[string]int64) {
		t.Helper()
		for agent, bytes := range agents {
			if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agent_minutes (host, agent_id, minute, mem_max) VALUES ('host-1', $1, $2, $3)`,
				agent, at, bytes); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Two minutes on, all three are going: 6 of 19 GiB used.
	clock.Add(2 * time.Minute)
	at := clock.Now()
	resident(at, map[string]int64{"orch-long": 1229 << 20, "orch-s1": 614 << 20, "orch-s2": 614 << 20})
	memMinute(t, b, at, 19*gib, 13*gib)

	head := startRun(t, h, heavy, "second heavy run")
	if got := runField(t, h, head, "status"); got != "queued" {
		t.Fatalf("head = %v: two heavy runs' peaks cannot meet on a 19 GiB host", got)
	}
	if reason := runField(t, h, head, "queue_reason").(string); !strings.Contains(reason, "should its peak meet the largest to come of the runs going") || !strings.Contains(reason, "8.9 GiB for /w/pdw") {
		t.Errorf("head's reason = %q", reason)
	}
	var small []string
	for i := range 6 {
		clock.Add(time.Second)
		small = append(small, startRun(t, h, finder, fmt.Sprintf("small task %d", i)))
	}
	// 6 GiB used and a 7.7 GiB peak still to come for the long run: four finders' starts fit
	// beside it under 95% of 19 GiB, a fifth does not.
	for i, want := range []string{"finding_project", "finding_project", "finding_project", "finding_project", "queued", "queued"} {
		if got := runField(t, h, small[i], "status"); got != want {
			t.Fatalf("small task %d = %v, want %v: %v", i, got, want, runField(t, h, small[i], "queue_reason"))
		}
	}
	if reason := runField(t, h, small[4], "queue_reason").(string); !strings.Contains(reason, "memory") {
		t.Errorf("fifth small task's reason = %q, want its own memory reason", reason)
	}
	code, r := sqlPost(t, h, q(`SELECT text FROM workflow_run_events WHERE run_id = '`+small[0]+`' AND kind = 'dequeued'`))
	if code != http.StatusOK || len(r.Rows) != 0 {
		t.Errorf("a run that never queued has a dequeued event: %+v", r)
	}
	for _, agent := range []string{"finder-0", "finder-1", "finder-2", "finder-3"} {
		succeed(t, b, b.command(), `{"agentId":"`+agent+`"}`)
	}

	// The long run ends. A minute on, the finders use 1.5 GiB each: the head would fit, were it not
	// for them, so the queue holds for it, and the small tasks behind it wait, though they fit.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded', finished_at=$2 WHERE id=$1`, long, clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	at = clock.Now()
	resident(at, map[string]int64{"orch-s1": 614 << 20, "orch-s2": 614 << 20,
		"finder-0": 1536 << 20, "finder-1": 1536 << 20, "finder-2": 1536 << 20, "finder-3": 1536 << 20})
	memMinute(t, b, at, 19*gib, 19*gib-11059<<20) // 10.8 GiB used
	if got := runField(t, h, head, "status"); got != "queued" {
		t.Fatalf("head = %v, want queued: the finders hold its room", got)
	}
	for _, id := range small[4:] {
		if got := runField(t, h, id, "status"); got != "queued" {
			t.Fatalf("a small task overtook the head the finders are holding: %v", got)
		}
		if reason := runField(t, h, id, "queue_reason").(string); !strings.Contains(reason, "the queue holds for") || !strings.Contains(reason, "heavy run") {
			t.Errorf("held reason = %q", reason)
		}
	}

	// The finders end; the next minute shows their memory gone. The head starts, and the small
	// tasks behind it fit beside it.
	for _, id := range small[:4] {
		if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+id+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
			t.Fatal(rec.Body)
		}
	}
	clock.Add(time.Minute)
	at = clock.Now()
	resident(at, map[string]int64{"orch-s1": 614 << 20, "orch-s2": 614 << 20})
	memMinute(t, b, at, 19*gib, 19*gib-4915<<20) // 4.8 GiB used
	if got := runField(t, h, head, "status"); got == "queued" {
		t.Fatalf("head still queued: %v", runField(t, h, head, "queue_reason"))
	}
	if got := runField(t, h, small[4], "status"); got != "finding_project" {
		t.Fatalf("small task behind the head = %v: %v", got, runField(t, h, small[4], "queue_reason"))
	}
}

// Every queued run's reason is kept current, not only the head's: a run that was fifth in line
// when it came does not still say so when it is second.
func TestEveryQueuedRunsReasonIsKeptCurrent(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h)
	hostMinute(t, b, clock.Now().Add(-time.Minute), 97, 0.3)
	var runs []string
	for i := range 3 {
		clock.Add(time.Second)
		runs = append(runs, startRun(t, h, wf, fmt.Sprintf("task %d", i)))
	}
	hostMinute(t, b, clock.Now(), 96, 0.3)
	for _, id := range runs {
		if reason := runField(t, h, id, "queue_reason").(string); !strings.Contains(reason, "CPU 96% now") {
			t.Errorf("reason = %q, want the newest minute's CPU", reason)
		}
	}
}

// rawMinute records host-1's minute at m straight into host_minutes, without the stream (whose
// minute starts the queue at once), so a test can start the queue itself.
func rawMinute(t *testing.T, d dbtest.DB, m time.Time, cpu float64, total, available int64) {
	t.Helper()
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
		VALUES ('host-1', $1, 12, 8, $2, 1, $3, $4)`, m, cpu, total, available); err != nil {
		t.Fatal(err)
	}
}

// A run canceled while the queue is choosing it is not started: the queue reads its runs without
// locking them, so it locks the one it picks only while that one is still queued.
func TestARunCanceledAsTheQueuePicksItIsNotStarted(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h)
	hostMinute(t, b, clock.Now().Add(-time.Minute), 97, 0.3)
	id := startRun(t, h, wf, "canceled as it is picked")
	if got := runField(t, h, id, "status"); got != "queued" {
		t.Fatalf("status = %v, want queued", got)
	}
	rawMinute(t, d, clock.Now(), 10, 100*gib, 70*gib) // room now

	// Cancel holds the run's row while the queue reads it, and commits once the queue waits for it.
	cancel, err := d.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel.Rollback(t.Context())
	if _, err := cancel.Exec(t.Context(), "SELECT 1 FROM workflow_runs WHERE id = $1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	e := workflow.NewEngine(d.Owner, clock.Now)
	done := make(chan error, 1)
	go func() { done <- e.StartQueued(t.Context()) }()
	for waited := 0; ; waited++ {
		var n int
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the queue finished without waiting for the run's row: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if waited > 1000 {
			t.Fatal("the queue never waited for the run's row")
		}
	}
	if _, err := cancel.Exec(t.Context(), "UPDATE workflow_runs SET status = 'canceled', finished_at = $2, queue_reason = '' WHERE id = $1",
		id, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cancel.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, id, "status"); got != "canceled" {
		t.Fatalf("a canceled run was started: %v", got)
	}
	code, r := sqlPost(t, h, q(`SELECT count(*) FROM workflow_run_events WHERE run_id = '`+id+`' AND kind = 'dequeued'`))
	if code != http.StatusOK || r.Rows[0][0] != float64(0) {
		t.Fatalf("a canceled run was dequeued: %+v", r)
	}
}

// A start retry the queue cannot send yet (here overdue: reconciliation sends it or fails the run)
// does not hold the runs behind it: one that fits starts. The retry keeps its memory, so the run
// after that, which would fit beside the first without it, waits.
func TestAHeldStartRetryDoesNotHoldTheRunsBehindIt(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h) // no history: 4 GiB each
	rawMinute(t, d, clock.Now(), 10, 16*gib, 16*gib)
	retry := startRun(t, h, wf, "its start failed")
	cmd := b.command()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// Its retry is queued and RetryWaitLimit overdue: the queue itself cannot send it.
	clock.Add(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET start_retry_at = $2, start_retry_queued = true, pending_command = launch_command
		WHERE id = $1`, retry, clock.Now().Add(-workflow.RetryWaitLimit-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 7 of 16 GiB used, and busy CPU for now, so the next two runs queue behind the retry.
	rawMinute(t, d, clock.Now(), 97, 16*gib, 9*gib)
	clock.Add(time.Second)
	next := startRun(t, h, wf, "fits beside the retry")
	clock.Add(time.Second)
	after := startRun(t, h, wf, "fits only were the retry's memory not counted")
	for _, id := range []string{next, after} {
		if got := runField(t, h, id, "status"); got != "queued" {
			t.Fatalf("status = %v, want queued while the CPU is busy", got)
		}
	}
	// The CPU calms: the next run starts past the held retry; the one after it does not fit beside
	// the retry's 4 GiB and the next run's (100% should their peaks meet).
	clock.Add(time.Minute)
	rawMinute(t, d, clock.Now(), 10, 16*gib, 9*gib)
	if err := workflow.NewEngine(d.Owner, clock.Now).StartQueued(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, next, "status"); got == "queued" {
		t.Fatalf("the held retry kept the next run queued: %v", runField(t, h, next, "queue_reason"))
	}
	if got := runField(t, h, after, "status"); got != "queued" {
		t.Fatalf("the run after = %v, want queued: the retry's memory must stay counted", got)
	}
	if reason := runField(t, h, after, "queue_reason").(string); !strings.Contains(reason, "memory") {
		t.Errorf("reason = %q, want memory", reason)
	}
	if got := runField(t, h, retry, "start_retry_queued"); got != true {
		t.Errorf("the retry left the queue: %v", got)
	}
}
