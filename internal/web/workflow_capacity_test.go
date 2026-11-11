package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowRepositoryCapacityGate(t *testing.T) {
	_, d := dbServer(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = func() time.Time { return t0 } })
	sendReport(t, h, wfReport())
	ctx := context.Background()
	const gib = int64(1 << 30)
	if _, err := d.Owner.Exec(ctx, `INSERT INTO host_minutes (host, minute, samples, mem_total, mem_available_min)
		VALUES ('host-1', TIMESTAMPTZ '2026-11-08 12:00:00+00', 1, $1, $2)`, 16*gib, 7*gib); err != nil {
		t.Fatal(err)
	}
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/heavy"}
	heavy := createWorkflow(t, h, def)
	// A completed run with two simultaneous agents used 8 GiB, not just the largest agent.
	history := startRun(t, h, heavy, "historical run")
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_runs SET status = 'succeeded', finished_at = TIMESTAMPTZ '2026-11-08 12:00:00+00' WHERE id = $1;
	`, history); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"a", "b"} {
		if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_run_agents (run_id, host, agent_id, agent, workspace, captured_at)
			VALUES ($1, 'host-1', $2, '{}', '{}', TIMESTAMPTZ '2026-11-08 12:00:00+00');`, history, agent); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, `INSERT INTO agent_minutes (host, agent_id, minute, mem_max) VALUES ('host-1', $1, TIMESTAMPTZ '2026-11-08 12:00:00+00', $2)`, agent, 4*gib); err != nil {
			t.Fatal(err)
		}
	}
	queued := startRun(t, h, heavy, "heavy job")
	if got := readRun(t, h, queued)["status"]; got != "queued" {
		t.Fatalf("heavy status = %v, want queued", got)
	}
	def.Project.Directory = "/w/light"
	light := createWorkflow(t, h, def)
	admitted := startRun(t, h, light, "light job")
	if got := readRun(t, h, admitted)["status"]; got != "starting" {
		t.Fatalf("light status = %v", got)
	}
	second := startRun(t, h, light, "another light job")
	if got := readRun(t, h, second)["status"]; got != "queued" {
		t.Fatalf("reservation lost: %v", got)
	}
	code, result := sqlPost(t, h, q(`SELECT estimated_memory_bytes FROM workflow_runs WHERE id = '`+queued+`'`))
	if code != http.StatusOK || len(result.Rows) != 1 || result.Rows[0][0] != float64(10*gib) {
		t.Fatalf("estimate: %d %+v", code, result)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+second+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel queued run: %d %s", rec.Code, rec.Body)
	}
	// Re-evaluate on the next resource report, including after an engine restart.
	if _, err := d.Owner.Exec(ctx, `UPDATE host_minutes SET mem_available_min = $1`, 16*gib); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewEngine(d.Owner, func() time.Time { return t0 }).AdmitHost(ctx, "host-1"); err != nil {
		t.Fatal(err)
	}
	if got := readRun(t, h, queued)["status"]; got != "starting" {
		t.Fatalf("did not drain: %v", got)
	}
	if got := readRun(t, h, second)["status"]; got != "canceled" {
		t.Fatalf("canceled run admitted: %v", got)
	}
}

func TestWorkflowCapacityWaitsForFreshResourcesAndReservesConcurrentStarts(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	ctx := context.Background()
	const gib = int64(1 << 30)
	if _, err := d.Owner.Exec(ctx, `INSERT INTO host_minutes (host, minute, samples, mem_total, mem_available_min)
		VALUES ('host-1', now() - interval '10 minutes', 1, $1, $1)`, 8*gib); err != nil {
		t.Fatal(err)
	}
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	wf := createWorkflow(t, h, def)
	ids := make(chan string, 2)
	for range 2 {
		go func() { ids <- startRun(t, h, wf, "parallel task") }()
	}
	first, second := <-ids, <-ids
	for _, id := range []string{first, second} {
		if got := readRun(t, h, id)["status"]; got != "queued" {
			t.Fatalf("stale sample admitted run: %v", got)
		}
	}
	// A fresh host frame drains exactly one run (4 GiB each, with 1 GiB left free).
	ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &resources.Report{
		Minutes: []resources.Minute{{Minute: time.Now().UTC().Truncate(time.Minute), Samples: 1, MemTotal: uint64(8 * gib), MemAvailableMin: uint64(8 * gib)}},
	}})
	if ack.Error != "" {
		t.Fatal(ack.Error)
	}
	b.command()
	statuses := map[any]int{}
	for _, id := range []string{first, second} {
		statuses[readRun(t, h, id)["status"]]++
	}
	if statuses["queued"] != 1 || statuses["starting"] != 1 {
		t.Fatalf("admissions = %v", statuses)
	}
}
