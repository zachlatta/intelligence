package web

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRetryEventsReadNaturally(t *testing.T) {
	for kind, want := range map[string]string{"start_retry": "Start failed; retry queued: daemon unavailable", "start_retry_sent": "Retry sent: Retry 1 of 3"} {
		text := "daemon unavailable"
		if kind == "start_retry_sent" {
			text = "Retry 1 of 3"
		}
		if got := (runEvent{Kind: kind, Text: text}).Line(); got != want {
			t.Fatalf("%s = %q, want %q", kind, got, want)
		}
	}
}

func TestRetryPageShowsProgressAndRecoveryWait(t *testing.T) {
	h, db := dbServer(t)
	rep := wfReport()
	b := sendReport(t, h, rep)
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "visible retry")
	c := b.command()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed, Error: "daemon <unavailable>"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var due time.Time
	if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
		t.Fatal(err)
	}
	check := func(wants ...string) {
		t.Helper()
		page := get(t, h, "/workflow-runs/"+run, nil)
		if page.Code != 200 {
			t.Fatal(page.Body)
		}
		for _, want := range wants {
			if !strings.Contains(page.Body.String(), want) {
				t.Fatalf("page missing %q", want)
			}
		}
	}
	check("Retry 1 of 3 queued", due.Format("Jan 2, 2006 15:04:05 MST"), "daemon &lt;unavailable&gt;", "Start failed; retry queued:")
	rep.Paseo.Connected = false
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	check("Waiting for the Paseo daemon to reconnect", "24 hours")
	rep.Paseo.Connected = true
	rep.StartRetryKeys = false
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	check("Waiting for the host to support retry keys")
	rep.StartRetryKeys = true
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	e := workflow.NewEngine(db.Owner, func() time.Time { return due })
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := b.command()
	check("Retry 1 of 3 sent", "Retry sent: Retry 1 of 3", "daemon &lt;unavailable&gt;")
	succeed(t, b, next, `{"agentId":"finder"}`)
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if strings.Contains(page, `id="start-retry"`) {
		t.Fatal("retry banner survives success")
	}
}

func TestRetryWaitExpiresAfter24Hours(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon", true: "host upgrade"}[connected], func(t *testing.T) {
			h, db := dbServer(t)
			rep := wfReport()
			b := sendReport(t, h, rep)
			run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "wait deadline")
			c := b.command()
			if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed, Error: "last start error"}}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			var due time.Time
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
				t.Fatal(err)
			}
			rep.Paseo.Connected = connected
			rep.StartRetryKeys = false
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			at := due.Add(24*time.Hour - time.Second)
			e := workflow.NewEngine(db.Owner, func() time.Time { return at })
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r := readRun(t, h, run); r["status"] == "failed" {
				t.Fatal("failed before deadline")
			}
			at = due.Add(24 * time.Hour)
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			r := readRun(t, h, run)
			if r["status"] != "failed" || !strings.Contains(r["error"].(string), "24 hours") || !strings.Contains(r["error"].(string), "last start error") {
				t.Fatal(r)
			}
		})
	}
}

func TestRetryReconciliationContinuesAfterBadRun(t *testing.T) {
	h, db := dbServer(t)
	b := sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	bad := startRun(t, h, wf, "bad run")
	badCmd := b.command()
	good := startRun(t, h, wf, "good run")
	goodCmd := b.command()
	due := time.Now().Add(time.Hour)
	for _, c := range []hostclient.Command{badCmd, goodCmd} {
		if _, err := db.Owner.Exec(t.Context(), "UPDATE commands SET status='failed',error='daemon unavailable' WHERE id=$1", c.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET start_retry_at=$1 WHERE id IN ($2,$3)", due, bad, good); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET host='missing-host',created_at=created_at-interval '1 day' WHERE id=$1", bad); err != nil {
		t.Fatal(err)
	}
	e := workflow.NewEngine(db.Owner, func() time.Time { return due })
	if err := e.RetryStarts(t.Context()); err == nil {
		t.Fatal("bad run error not reported")
	}
	var retries int
	if err := db.Owner.QueryRow(t.Context(), "SELECT start_retries FROM workflow_runs WHERE id=$1", good).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 1 {
		t.Fatalf("bad run blocked healthy run: retries=%d", retries)
	}
}
