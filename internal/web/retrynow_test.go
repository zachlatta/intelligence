package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// A queued start retry can be started by hand, ahead of the capacity queue. It spends a retry like
// any other attempt, and a failure is handled like any other failed retry.
func TestStartRetryNowSkipsQueue(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) {
		c.Now = clock.Now
		c.QueueInterval = time.Hour
	})
	b := sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "First run")
	original := b.command()
	fail := func(id string) {
		t.Helper()
		ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{
			ID: id, Status: hostclient.CommandFailed, Error: "temporary startup error",
		}})
		if ack.Error != "" {
			t.Fatal(ack.Error)
		}
	}
	start := func() int {
		t.Helper()
		rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/start", `{}`, sameOriginFetch)
		return rec.Code
	}
	hostMinute(t, b, clock.Now().Truncate(time.Minute), 95, 0.3)
	fail(original.ID)

	// In its backoff, the retry is not in the queue yet: there is nothing to start.
	if page := get(t, h, "/workflow-runs/"+run, nil).Body.String(); strings.Contains(page, `id="start-now"`) {
		t.Error("a retry still in its backoff should not offer Start retry now")
	}
	if code := start(); code != http.StatusConflict {
		t.Fatalf("starting a retry in its backoff = %d", code)
	}

	clock.Add(15 * time.Second)
	e := workflow.NewEngine(d.Owner, clock.Now)
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if !strings.Contains(page, "Retry 1 of 3 queued") || !strings.Contains(page, `id="start-now"`) || !strings.Contains(page, "Start retry now") {
		t.Fatal("a queued retry should offer Start retry now")
	}

	if code := start(); code != http.StatusNoContent {
		t.Fatalf("start retry now = %d", code)
	}
	launch, _ := runField(t, h, run, "launch_command").(string)
	if launch == "" || launch == original.ID || launch != runField(t, h, run, "pending_command") {
		t.Fatalf("forced retry sent the wrong attempt: %v", launch)
	}
	var key string
	if err := d.Owner.QueryRow(t.Context(), "SELECT idempotency_key FROM commands WHERE id=$1", launch).Scan(&key); err != nil || key != original.ID {
		t.Fatalf("forced retry key = %q, %v", key, err)
	}
	if n := runField(t, h, run, "start_retries"); n != float64(1) {
		t.Fatalf("a forced retry should count as a retry: %v", n)
	}
	if byHand := runField(t, h, run, "started_by_hand"); byHand != true {
		t.Fatalf("a forced retry should mark the run started by hand, as Start now does: %v", byHand)
	}
	if q := runField(t, h, run, "start_retry_queued"); q != false {
		t.Fatalf("the forced retry is still queued: %v", q)
	}
	var queued int
	if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_queue WHERE run_id=$1", run).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("the forced retry is still in the queue: %d %v", queued, err)
	}
	page = get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if !strings.Contains(page, "Started: retry started by hand, ahead of the queue") || !strings.Contains(page, "Retry 1 of 3 sent") {
		t.Fatal("the timeline should say the retry was started by hand")
	}
	if code := start(); code != http.StatusConflict {
		t.Fatalf("starting a retry that was already sent = %d", code)
	}

	// The forced attempt fails like any other: the next retry is scheduled with the next backoff.
	fail(launch)
	if n := runField(t, h, run, "start_retries"); n != float64(1) {
		t.Fatalf("a failed forced retry changed the count: %v", n)
	}
	if at, _ := runField(t, h, run, "start_retry_at").(string); at == "" {
		t.Fatal("a failed forced retry should schedule the next retry")
	}
	if page := get(t, h, "/workflow-runs/"+run, nil).Body.String(); !strings.Contains(page, "Retry 2 of 3 queued") {
		t.Fatal("the next retry should be Retry 2 of 3")
	}
}
