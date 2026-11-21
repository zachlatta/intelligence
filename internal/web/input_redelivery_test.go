package web

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// failCommand reports cmd failed with why, as the host does when its Paseo daemon is down.
func failCommand(t *testing.T, b *rawHost, cmd hostclient.Command, why string) {
	t.Helper()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed,
		Error: why}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

func commandText(t *testing.T, c hostclient.Command) string {
	t.Helper()
	var a hostclient.AgentArgs
	if err := json.Unmarshal(c.Args, &a); err != nil {
		t.Fatal(err)
	}
	return a.Text
}

// Zach's reply recorded while the host could not reach its Paseo daemon failed to send, and the
// orchestrator, nudged by the watchdog, asked the same question again: the reply was lost (run
// 2c75da8c, 2026-11-19). A reply whose delivery failed goes to the orchestrator with the next
// thing that reaches it, once, and it cannot ask the question the reply answered.
func TestAReplyThatFailedToSendReachesTheOrchestratorOnce(t *testing.T) {
	f := newStallFixture(t)
	const reply = "Sounds good. Make sure every export that can be hardware accelerated is, then land it"
	for _, next := range []string{"continuation", "ask", "message"} {
		t.Run(next, func(t *testing.T) {
			b := f.reset(t)
			d, clock, h := f.d, f.clock, f.h
			key := keyOf(t, h, "host-1")
			run := startRun(t, h, fixedWorkflow(t, h), "update the archive")
			succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
			if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "Land the exporter?"}); code != 200 {
				t.Fatalf("ask: %d %s", code, msg)
			}
			if r := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", `{"text":"`+reply+`"}`, sameOriginFetch); r.Code != 204 {
				t.Fatalf("reply: %d %s", r.Code, r.Body)
			}
			sent := b.command()
			if sent.Kind != hostclient.SendMessageKind || !strings.Contains(commandText(t, sent), reply) {
				t.Fatalf("reply command: %+v", sent)
			}
			failCommand(t, b, sent, "not connected to the Paseo daemon")

			engine := workflow.NewEngine(d.Owner, clock.Now)
			rep := wfReport()
			rep.Paseo.Agents = []hostclient.Agent{{ID: "orch-1", WorkspaceID: "w-run", Status: "idle", RequiresAttention: true, AttentionReason: "finished"}}
			reconcile := func() {
				t.Helper()
				if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
					t.Fatal(ack.Error)
				}
				if err := engine.ReconcileStalls(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			switch next {
			case "continuation":
				reconcile()
				clock.Add(2 * time.Minute)
				reconcile()
				c := b.command()
				text := commandText(t, c)
				if c.Kind != hostclient.SendMessageKind || !strings.Contains(text, reply) || !strings.Contains(text, "Continue this workflow") {
					t.Fatalf("continuation: %+v", c)
				}
				if !strings.Contains(text, "Zach's reply to your note") || !strings.Contains(text, "Do not ask it again") {
					t.Errorf("continuation does not mark the reply as the answer to the open ask:\n%s", text)
				}
				// A send that expired may still have run: the orchestrator must not repeat what it did.
				if !strings.Contains(text, "may not have reached you") || !strings.Contains(text, "do not do it again") {
					t.Errorf("continuation does not warn against repeating an action already taken:\n%s", text)
				}
				if strings.Index(text, reply) > strings.Index(text, "Continue this workflow") {
					t.Errorf("the reply comes after the generic continuation:\n%s", text)
				}
				succeed(t, b, c, `{}`)
				clock.Add(10 * time.Minute)
				reconcile()
				if again := commandText(t, b.command()); strings.Contains(again, reply) {
					t.Errorf("a delivered reply was sent again:\n%s", again)
				}
			case "ask":
				code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "Re-sent: Land the exporter?"})
				if code != 200 || !strings.Contains(msg, reply) || !strings.Contains(msg, "already answered") {
					t.Fatalf("re-ask: %d %s", code, msg)
				}
				if !strings.Contains(msg, "Do not end your turn") {
					t.Errorf("the refused ask lets the orchestrator end its turn:\n%s", msg)
				}
				if r := readRun(t, h, run); r["status"] != workflow.Running || r["note"] != "" {
					t.Fatalf("a re-ask of an answered question went to Zach: %v", r)
				}
				var asks int
				if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_run_events WHERE run_id=$1 AND kind='ask'", run).Scan(&asks); err != nil || asks != 1 {
					t.Fatalf("asks = %d (%v), want 1", asks, err)
				}
				if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "A new question"}); code != 200 || strings.Contains(msg, reply) {
					t.Fatalf("a later ask: %d %s", code, msg)
				}
			case "message":
				e := workflow.NewEngine(d.Owner, clock.Now)
				if err := pgx.BeginFunc(t.Context(), d.Owner, func(tx pgx.Tx) error {
					return e.MessageTx(t.Context(), tx, h.acct.ID, run, "A verified email reply", "Also an 8 GB limit per file")
				}); err != nil {
					t.Fatal(err)
				}
				text := commandText(t, b.command())
				if !strings.Contains(text, reply) || !strings.Contains(text, "Also an 8 GB limit per file") {
					t.Fatalf("new input without the lost reply:\n%s", text)
				}
			}
			code, r := sqlPost(t, h, q(`SELECT count(*) FILTER (WHERE delivered), count(*) FROM workflow_run_inputs WHERE run_id='`+run+`'`))
			if code != 200 || len(r.Rows) != 1 || r.Rows[0][1] == float64(0) {
				t.Fatalf("inputs: %d %+v", code, r)
			}
			if err := db.CheckContract(context.Background(), d.Owner, d.Readonly); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The migration links a live run's earlier replies and messages to the send_message made with
// each, as Reply and MessageTx now record them, so a reply lost before the deploy goes again too.
func TestInputDeliveryMigrationLinksEarlierInputs(t *testing.T) {
	f := newStallFixture(t)
	b := f.reset(t)
	d, clock, h := f.d, f.clock, f.h
	run := startRun(t, h, fixedWorkflow(t, h), "update the archive")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "ask", "note": "Land it?"}); code != 200 {
		t.Fatalf("ask: %d %s", code, msg)
	}
	if r := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", `{"text":"Land it"}`, sameOriginFetch); r.Code != 204 {
		t.Fatalf("reply: %d %s", r.Code, r.Body)
	}
	e := workflow.NewEngine(d.Owner, clock.Now)
	if err := pgx.BeginFunc(t.Context(), d.Owner, func(tx pgx.Tx) error {
		return e.MessageTx(t.Context(), tx, h.acct.ID, run, "A verified email reply", "And an 8 GB limit")
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	rows, err := d.Owner.Query(t.Context(), "SELECT event_id::text, command_id::text FROM workflow_run_input_deliveries WHERE run_id=$1", run)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ev, c string
		if err := rows.Scan(&ev, &c); err != nil {
			t.Fatal(err)
		}
		want[ev] = c
	}
	if len(want) != 2 {
		t.Fatalf("recorded inputs = %v, want a reply and a message", want)
	}
	sql, err := os.ReadFile("../db/migrations/20261120T1048_input_delivery.sql")
	if err != nil {
		t.Fatal(err)
	}
	const insert = "\nINSERT INTO workflow_run_input_deliveries"
	_, backfill, ok := strings.Cut(string(sql), insert)
	if !ok {
		t.Fatal("no backfill in the migration")
	}
	backfill = insert + backfill
	if _, err := d.Owner.Exec(t.Context(), "DELETE FROM workflow_run_input_deliveries"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), backfill); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err = d.Owner.Query(t.Context(), "SELECT event_id::text, command_id::text FROM workflow_run_input_deliveries WHERE run_id=$1 AND via='original'", run)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ev, c string
		if err := rows.Scan(&ev, &c); err != nil {
			t.Fatal(err)
		}
		got[ev] = c
	}
	if len(got) != len(want) {
		t.Fatalf("backfill = %v, want %v", got, want)
	}
	for ev, c := range want {
		if got[ev] != c {
			t.Errorf("event %s: backfill linked %s, want %s", ev, got[ev], c)
		}
	}

	// A send past its expiry is no longer pending, as undeliveredInputs has it, even before the
	// sweep marks it expired.
	if _, err := d.Owner.Exec(t.Context(), "UPDATE commands SET expires_at = now() - interval '1 minute' WHERE id::text = ANY($1)", slices.Collect(maps.Values(want))); err != nil {
		t.Fatal(err)
	}
	code, r := sqlPost(t, h, q(`SELECT count(*) FILTER (WHERE pending), count(*) FILTER (WHERE NOT delivered) FROM workflow_run_inputs WHERE run_id='`+run+`'`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != float64(0) || r.Rows[0][1] != float64(2) {
		t.Fatalf("expired sends: %d %+v, want no pending and two undelivered inputs", code, r)
	}
}
