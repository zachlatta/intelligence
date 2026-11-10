package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStartRetryWaitsForDaemonAndCanBeCanceledOrSucceed(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "cancel"}[cancelRun], func(t *testing.T) {
			h, db := dbServer(t)
			rep := wfReport()
			rep.StartRetryKeys = true
			b := sendReport(t, h, rep)
			def := workflow.Starting
			def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			run := startRun(t, h, createWorkflow(t, h, def), "recover")
			c := b.command()
			if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			var due time.Time
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
				t.Fatal(err)
			}
			rep.Paseo.Connected = false
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			e := workflow.NewEngine(db.Owner, func() time.Time { return due })
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			var retries int
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retries FROM workflow_runs WHERE id=$1", run).Scan(&retries); err != nil {
				t.Fatal(err)
			}
			if retries != 0 {
				t.Fatalf("consumed retry while daemon unavailable: %d", retries)
			}
			if cancelRun {
				if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code != 204 {
					t.Fatal(rec.Body)
				}
			}
			rep.Paseo.Connected = true
			rep.StartRetryKeys = false
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retries FROM workflow_runs WHERE id=$1", run).Scan(&retries); err != nil {
				t.Fatal(err)
			}
			if retries != 0 {
				t.Fatal("retry sent to a host without creation-key support")
			}
			rep.StartRetryKeys = true
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if cancelRun {
				if r := readRun(t, h, run); r["status"] != "canceled" {
					t.Fatal(r)
				}
				return
			}
			next := b.command()
			succeed(t, b, next, `{"workspaceId":"recovered","agentId":"orch"}`)
			// A delayed report from an older delivery cannot replace the newer outcome.
			succeed(t, b, c, `{"workspaceId":"old","agentId":"old"}`)
			if r := readRun(t, h, run); r["status"] != "running" || r["orchestrator_id"] != "orch" || r["error"] != "" {
				t.Fatal(r)
			}
		})
	}
}

func TestWorkflowStartRetriesDurably(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "finder", true: "orchestrator"}[fixed], func(t *testing.T) {
			h, db := dbServer(t)
			b := sendReport(t, h, wfReport())
			def := workflow.Starting
			if fixed {
				def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			}
			run := startRun(t, h, createWorkflow(t, h, def), "retry this")
			cmd := b.command()
			original := cmd.ID
			for attempt := 0; attempt < 4; attempt++ {
				var result json.RawMessage
				if fixed && attempt == 0 {
					result = json.RawMessage(`{"workspaceId":"partial"}`)
				}
				if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Result: result, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				r := readRun(t, h, run)
				if attempt == 3 {
					if r["status"] != "failed" || !strings.Contains(r["error"].(string), "not connected") {
						t.Fatal(r)
					}
					if fixed {
						cleanup := b.command()
						if cleanup.Kind != hostclient.ArchiveWorkspaceKind || !strings.Contains(string(cleanup.Args), "partial") {
							t.Fatal(cleanup)
						}
					}
					break
				}
				if r["status"] == "failed" {
					t.Fatalf("failed before retries: %v", r)
				}
				var due time.Time
				if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
					t.Fatal(err)
				}
				var scheduled time.Time
				if err := db.Owner.QueryRow(t.Context(), "SELECT updated_at FROM workflow_runs WHERE id=$1", run).Scan(&scheduled); err != nil {
					t.Fatal(err)
				}
				if delay := due.Sub(scheduled); delay != 15*time.Second*time.Duration(1<<attempt) {
					t.Fatalf("backoff %d = %v", attempt, delay)
				}
				// A fresh engine represents a server restart; concurrent engines cannot enqueue twice.
				early := workflow.NewEngine(db.Owner, func() time.Time { return due.Add(-time.Second) })
				if err := early.RetryStarts(t.Context()); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := db.Owner.QueryRow(t.Context(), "SELECT count(*) FROM commands WHERE kind=$1", cmd.Kind).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != attempt+1 {
					t.Fatalf("retried before backoff: %d", count)
				}
				var wg sync.WaitGroup
				for range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						e := workflow.NewEngine(db.Owner, func() time.Time { return due })
						if err := e.RetryStarts(t.Context()); err != nil {
							t.Error(err)
						}
					}()
				}
				wg.Wait()
				next := b.command()
				if next.ID == cmd.ID || next.IdempotencyKey != original {
					t.Fatalf("retry identity: %+v", next)
				}
				cmd = next
			}
		})
	}
}
