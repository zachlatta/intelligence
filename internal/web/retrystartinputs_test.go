package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestStartRetryRebuildsInputAndResolvesImages(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		fixed, image bool
	}{{"finder image", false, true}, {"orchestrator image", true, true}, {"finder catalog", false, false}} {
		fixed := scenario.fixed
		t.Run(scenario.name, func(t *testing.T) {
			h, db := dbServer(t)
			rep := wfReport()
			rep.Paseo.Connected = false
			rep.Paseo.Workspaces = nil
			rep.Paseo.Projects = nil
			sendReport(t, h, rep)
			b := openStream(t, h, "host-1")
			var img bytes.Buffer
			if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"name": "screen.png", "data": base64.StdEncoding.EncodeToString(img.Bytes())})
			upload := call(t, h, "POST", "/api/attachments", string(body), sameOriginFetch)
			if upload.Code != 201 {
				t.Fatal(upload.Body)
			}
			var attachment struct{ Reference string }
			if err := json.Unmarshal(upload.Body.Bytes(), &attachment); err != nil {
				t.Fatal(err)
			}
			def := workflow.Starting
			if fixed {
				def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			}
			input := "Inspect task"
			if scenario.image {
				input += " " + attachment.Reference
			}
			run := startRun(t, h, createWorkflow(t, h, def), input)
			c := b.command()
			if !fixed && !strings.Contains(string(c.Args), "This host has no repositories") {
				t.Fatal("test did not start with empty catalog")
			}
			if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed, Error: "not connected to the Paseo daemon"}}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			var due time.Time
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET input=input || E'\\n\\nMore context from the thread' WHERE id=$1", run); err != nil {
				t.Fatal(err)
			}
			rep = wfReport()
			if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			e := workflow.NewEngine(db.Owner, func() time.Time { return due })
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			next := b.command()
			if next.ID == c.ID || next.IdempotencyKey != c.ID {
				t.Fatal("lost stable creation key")
			}
			var agent hostclient.CreateAgentArgs
			if fixed {
				var ws hostclient.CreateWorkspaceArgs
				if err := json.Unmarshal(next.Args, &ws); err != nil {
					t.Fatal(err)
				}
				if ws.Agent == nil {
					t.Fatal("no orchestrator")
				}
				agent = *ws.Agent
			} else {
				if err := json.Unmarshal(next.Args, &agent); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.image && (len(agent.Attachments) != 1 || !bytes.Equal(agent.Attachments[0].Data, img.Bytes())) {
				t.Fatal("retry lost image")
			}
			if strings.Contains(agent.Prompt, "[attachment:") || !strings.Contains(agent.Prompt, "More context from the thread") {
				t.Fatal("retry lost current input")
			}
			if !fixed && (!strings.Contains(agent.Prompt, "/w/app") || strings.Contains(agent.Prompt, "This host has no repositories")) {
				t.Fatal("retry used stale catalog")
			}
		})
	}
}

func TestOfflineStartupCommandsExpireAndReachDeadline(t *testing.T) {
	for _, status := range []string{"queued", "sent", "running"} {
		t.Run(status, func(t *testing.T) {
			h, db := dbServer(t)
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")
			run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "offline start")
			c := b.command()
			if _, err := db.Owner.Exec(t.Context(), "UPDATE commands SET status=$2 WHERE id=$1", c.ID, status); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Owner.Exec(t.Context(), "UPDATE hosts SET stream_id=NULL WHERE name='host-1'"); err != nil {
				t.Fatal(err)
			}
			at := c.ExpiresAt.Add(26 * time.Hour)
			e := workflow.NewEngine(db.Owner, func() time.Time { return at })
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := db.Owner.QueryRow(t.Context(), "SELECT status FROM commands WHERE id=$1", c.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != "expired" {
				t.Fatalf("offline command never expired: %s", got)
			}
			// A second reconciliation must honor the original expiry even if the server
			// first noticed it more than a day late.
			if err := e.RetryStarts(t.Context()); err != nil {
				t.Fatal(err)
			}
			r := readRun(t, h, run)
			if r["status"] != "failed" || !strings.Contains(r["error"].(string), "24 hours") {
				t.Fatal(r)
			}
		})
	}
}

func TestOfflineHostDoesNotConsumeRetryOrLookReady(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "no stream", true: "stale heartbeat"}[stale], func(t *testing.T) {
			h, db := dbServer(t)
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")
			run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "offline retry")
			c := b.command()
			if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed, Error: "daemon unavailable"}}); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			var due time.Time
			if err := db.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
				t.Fatal(err)
			}
			sql := "UPDATE hosts SET stream_id=NULL WHERE name='host-1'"
			if stale {
				sql = "UPDATE hosts SET last_seen=now()-interval '2 minutes' WHERE name='host-1'"
			}
			if _, err := db.Owner.Exec(t.Context(), sql); err != nil {
				t.Fatal(err)
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
				t.Fatal("offline host consumed retry")
			}
			page := get(t, h, "/workflow-runs/"+run, nil)
			if page.Code != 200 || !strings.Contains(page.Body.String(), "Waiting for the host to reconnect") {
				t.Fatal("offline host appears ready")
			}
		})
	}
}
