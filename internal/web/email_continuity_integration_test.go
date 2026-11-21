package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestEmailContinuationUsesMainStartupPolicy(t *testing.T) {
	for _, mode := range []string{"normal queue", "immediate priority", "fallback queue", "disabled provider", "credential", "cancellation blocks redelivery", "prior resolution", "late expiry"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			d := dbtest.New(t)
			h := newServer(t, d, d.Owner, "test", func(c *Config) { c.QueueInterval = time.Hour })
			b := sendReport(t, h, wfReport())
			def := workflow.Starting
			def.Trigger = workflow.TriggerEmail
			def.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
			def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			def.Priority = workflow.PriorityNormal
			if mode == "immediate priority" {
				def.Priority = workflow.PriorityImmediate
			}
			wf := createWorkflow(t, h, def)
			old := startRun(t, h, wf, "Initial request")
			succeed(t, b, b.command(), `{"workspaceId":"ws","agentId":"same-orchestrator"}`)
			if mode == "prior resolution" {
				rec := call(t, anon(h), "POST", "/api/workflow-runs/"+old+"/answer-resolution", `{"kind":"not_needed","reason":"Notification only"}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
				if rec.Code != 200 {
					t.Fatalf("resolution: %d %s", rec.Code, rec.Body)
				}
			}
			if code, msg := move(t, h, keyOf(t, h, "host-1"), old, map[string]string{"move": "done", "summary": "Original task finished"}); code != 200 {
				t.Fatalf("done %d %s", code, msg)
			}
			succeed(t, b, b.command(), `{"workspaceId":"ws"}`)
			now := time.Now().UTC()
			if mode == "normal queue" || mode == "immediate priority" {
				hostMinute(t, b, now.Truncate(time.Minute), 99, .3)
			}
			if mode == "disabled provider" {
				if _, err := d.Owner.Exec(t.Context(), "INSERT INTO provider_plan_settings(provider,enabled) VALUES('claude',false) ON CONFLICT(provider) DO UPDATE SET enabled=false"); err != nil {
					t.Fatal(err)
				}
			}
			input := "Follow-up request"
			if mode == "credential" {
				input = "Read this: ibk_" + strings.Repeat("a", 43)
			}
			e := workflow.NewEngine(d.Owner, nil)
			var id string
			err := pgx.BeginFunc(t.Context(), d.Owner, func(tx pgx.Tx) error {
				var err error
				id, _, err = e.ContinueEmailTx(t.Context(), tx, h.acct.ID, old, input, now)
				return err
			})
			if mode == "credential" {
				if err == nil || strings.Contains(err.Error(), input) {
					t.Fatal("credential was accepted or disclosed")
				}
				var count int
				if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_runs").Scan(&count); err != nil || count != 1 {
					t.Fatalf("partial credential run: %d %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := readContinuityRun(t, h, id)
			if r["answer_expected"] != true || r["project_selected"] != true || r["continued_run_id"] != old {
				t.Fatalf("metadata: %v", r)
			}
			switch mode {
			case "disabled provider":
				if r["status"] != workflow.Failed || !strings.Contains(r["error"].(string), "disabled") {
					t.Fatalf("disabled continuation: %v", r)
				}
				var count int
				if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM commands WHERE kind='continue_email'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("disabled resume: %d %v", count, err)
				}
			case "normal queue":
				if r["status"] != workflow.Queued || r["launched_at"] != nil {
					t.Fatalf("admission bypassed: %v", r)
				}
				hostMinute(t, b, now.Truncate(time.Minute).Add(time.Minute), 0, .3)
				if err := e.StartQueued(t.Context()); err != nil {
					t.Fatal(err)
				}
				cmd := b.command()
				if cmd.Kind != hostclient.ContinueEmailKind || !strings.Contains(string(cmd.Args), "same-orchestrator") {
					t.Fatalf("queued continuation lost context: %+v", cmd)
				}
			case "fallback queue":
				cmd := b.command()
				hostMinute(t, b, now.Truncate(time.Minute), 99, .3)
				ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Result: json.RawMessage(`{"agentGone":true}`), Error: "Agent not found"}})
				if ack.Error != "" {
					t.Fatal(ack.Error)
				}
				r = readContinuityRun(t, h, id)
				if r["status"] != workflow.Queued || r["orchestrator_id"] != "" {
					t.Fatalf("cold fallback bypassed admission: %v", r)
				}
				hostMinute(t, b, now.Truncate(time.Minute).Add(time.Minute), 0, .3)
				if err := e.StartQueued(t.Context()); err != nil {
					t.Fatal(err)
				}
				if cmd := b.command(); cmd.Kind != hostclient.CreateWorkspaceKind {
					t.Fatalf("fallback didn't launch fresh: %+v", cmd)
				}
			case "cancellation blocks redelivery":
				if err := e.Cancel(t.Context(), h.acct.ID, id); err != nil {
					t.Fatal(err)
				}
				commands, err := fleet.New(d.Owner).Claim(t.Context(), "host-1", fleet.ClaimOptions{Replay: true}, now)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range commands {
					if c.Kind == hostclient.ContinueEmailKind {
						t.Fatal("canceled follow-up was delivered")
					}
				}
			case "late expiry":
				cmd := b.command()
				if code, msg := move(t, h, keyOf(t, h, "host-1"), id, map[string]string{"move": "done", "summary": "Answered before receipt arrived"}); code != 200 {
					t.Fatalf("completion: %d %s", code, msg)
				}
				if err := fleet.New(d.Owner).ExpireCommands(t.Context(), []string{cmd.ID}, now.Add(25*time.Hour)); err != nil {
					t.Fatal(err)
				}
				if err := e.CommandUpdated(t.Context(), cmd.ID); err != nil {
					t.Fatal(err)
				}
				if readContinuityRun(t, h, id)["status"] != workflow.Succeeded {
					t.Fatal("late expiry reversed completion")
				}
				var launches int
				if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM commands WHERE kind='create_workspace'").Scan(&launches); err != nil || launches != 1 {
					t.Fatalf("late expiry launched another agent: %d %v", launches, err)
				}
			case "prior resolution":
				var unanswered bool
				if err := d.Owner.QueryRow(t.Context(), "SELECT answered_at IS NULL AND resolution IS NULL FROM workflow_run_answer_status WHERE id=$1", id).Scan(&unanswered); err != nil || !unanswered {
					t.Fatalf("inherited old resolution: %v %v", unanswered, err)
				}
			default:
				if r["status"] != workflow.Running {
					t.Fatalf("immediate continuation queued: %v", r)
				}
				if cmd := b.command(); cmd.Kind != hostclient.ContinueEmailKind {
					t.Fatalf("wrong immediate command: %+v", cmd)
				}
			}
			if err := db.CheckContract(context.Background(), d.Owner, d.Readonly); err != nil {
				t.Fatal(err)
			}
		})
	}
}
