package workflow

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
)

type stallRunStatus uint8

const (
	sRunning stallRunStatus = iota
	sWaiting
	sStalled
)

type replyDelivery uint8

const (
	rNone replyDelivery = iota
	rPending
	rFailed
	rDelivered
)

type stallState struct {
	status                stallRunStatus
	hasLifecycleEpoch     bool
	epoch                 uint8
	continuationsInEpoch  uint8
	daemonUp              bool
	reply                 replyDelivery
	askedAgainToZach      bool
	continuedWithoutEpoch bool
}

type stallTransition struct {
	name string
	next stallState
}

func stallNextStates(s stallState) []stallTransition {
	var out []stallTransition

	{
		n := s
		n.daemonUp = !s.daemonUp
		out = append(out, stallTransition{fmt.Sprintf("DaemonUp(%v)", n.daemonUp), n})
	}

	if s.status == sRunning && s.reply == rPending {
		n := s
		if s.daemonUp {
			n.reply = rDelivered
			out = append(out, stallTransition{"DeliverReply(succeeded)", n})
		} else {
			n.reply = rFailed
			out = append(out, stallTransition{"DeliverReply(failed-daemon-down)", n})
		}
	}

	if s.status == sRunning && s.reply != rPending {
		var undelivered []input
		if s.reply == rFailed {
			undelivered = []input{{Kind: "reply", Text: "proceed"}}
		}
		if answeredAsk(undelivered) {
			n := s
			n.reply = rDelivered
			out = append(out, stallTransition{"OrchestratorAsk(answered-with-undelivered-reply)", n})
		} else {
			n := s
			n.status = sWaiting
			out = append(out, stallTransition{"OrchestratorAsk(new-question)", n})
		}
	}

	if s.status == sWaiting && s.epoch < 2 {
		n := s
		n.status = sRunning
		n.hasLifecycleEpoch = true
		n.epoch++
		n.continuationsInEpoch = 0
		n.reply = rPending
		out = append(out, stallTransition{"ZachReply", n})
	}

	if s.status == sRunning && s.daemonUp && s.reply != rPending {
		for _, elapsed := range []time.Duration{time.Minute, 2 * time.Minute, 10 * time.Minute, 12 * time.Minute} {
			action, attempt := decideStallAction(s.hasLifecycleEpoch, true, int(s.continuationsInEpoch), elapsed)
			switch action {
			case stallContinue:
				n := s
				if !s.hasLifecycleEpoch {
					n.continuedWithoutEpoch = true
				}
				n.continuationsInEpoch = uint8(attempt)
				if n.reply == rFailed {
					n.reply = rDelivered
				}
				out = append(out, stallTransition{fmt.Sprintf("WatchdogContinue(%d@%s)", attempt, elapsed), n})
			case stallMarkStalled:
				n := s
				n.status = sStalled
				out = append(out, stallTransition{fmt.Sprintf("WatchdogMarkStalled(@%s)", elapsed), n})
			}
		}
	}

	if s.status == sStalled && s.daemonUp && s.epoch < 2 {
		n := s
		n.status = sRunning
		n.epoch++
		n.continuationsInEpoch = 1
		if n.reply == rFailed {
			n.reply = rDelivered
		}
		out = append(out, stallTransition{"Resume", n})
	}

	return out
}

func TestFormalModelStallsAndReplyRedelivery(t *testing.T) {
	initials := []stallState{
		{status: sRunning, hasLifecycleEpoch: true, epoch: 1, daemonUp: true},
		{status: sRunning, hasLifecycleEpoch: false, epoch: 0, daemonUp: true},
	}
	type node struct {
		state stallState
		trace []string
	}
	visited := make(map[stallState]bool)
	q := make([]node, 0, len(initials))
	for _, init := range initials {
		visited[init] = true
		q = append(q, node{state: init})
	}

	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		s := cur.state

		if s.askedAgainToZach {
			t.Fatalf("NoReaskWhenReplyUndelivered violated: %s", strings.Join(cur.trace, " -> "))
		}
		if s.continuedWithoutEpoch {
			t.Fatalf("NoContinuationWithoutLifecycleEpoch violated: %s", strings.Join(cur.trace, " -> "))
		}
		if s.continuationsInEpoch > 2 {
			t.Fatalf("BoundedContinuationsPerEpoch violated (%d > 2): %s", s.continuationsInEpoch, strings.Join(cur.trace, " -> "))
		}
		if s.status == sStalled && s.continuationsInEpoch != 2 {
			t.Fatalf("NoStallBeforeTwoContinuations violated (count=%d): %s", s.continuationsInEpoch, strings.Join(cur.trace, " -> "))
		}

		for _, tr := range stallNextStates(s) {
			if !visited[tr.next] {
				visited[tr.next] = true
				trace := append(append([]string(nil), cur.trace...), tr.name)
				q = append(q, node{state: tr.next, trace: trace})
			}
		}
	}
	if len(visited) < 20 {
		t.Fatalf("explored only %d states", len(visited))
	}
}

func setupModelEngine(t *testing.T) (*Engine, dbtest.DB, int64, *time.Time) {
	t.Helper()
	d := dbtest.New(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	eng := NewEngine(d.Owner, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	var acct int64
	if err := d.Owner.QueryRow(t.Context(), `INSERT INTO accounts (username, password_hash, role_name) VALUES ('modeluser', 'hash', 'ro_model') RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_keys (host, account_id, key_hash, created_at) VALUES ('host-1', $1, 'kh1', $2)`, acct, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO hosts (name, connected, status_error, stream_id, last_seen, report_received_at, start_retry_keys)
		VALUES ('host-1', true, '', '00000000-0000-4000-8000-000000000001', $1, $1, true)`, now); err != nil {
		t.Fatal(err)
	}
	return eng, d, acct, &now
}

func modelDef(provider string) Definition {
	d := Starting
	d.Host = "host-1"
	d.Project.Mode = ProjectFixed
	d.Project.Directory = "/w/app"
	d.Orchestrator.Provider = provider
	d.Orchestrator.Model = ""
	d.Orchestrator.Thinking = ""
	return d
}

// TestFormalModelAgainstRealEngine drives the real workflow.Engine against Postgres across
// concrete concurrency and state-machine interleavings corresponding to past production bugs:
//   - 450cced: unlocked queue scan interleaved with Cancel before row lock
//   - d36a461: concurrent and sequential RetryRun on the same failed run
//   - 016fe16 / 666da5e: ask while Zach's reply failed delivery
//   - c0d6440 / 149a0a0: stall watchdog with missing lifecycle epoch and bounded continuations
//   - 2381302: disabled-provider run at queue head must not block later queued run
func TestFormalModelAgainstRealEngine(t *testing.T) {
	t.Run("450cced_cancel_interleaved_with_StartQueued", func(t *testing.T) {
		eng, d, acct, now := setupModelEngine(t)
		wfID, err := eng.CreateWorkflow(t.Context(), acct, "WF", "", modelDef("claude"))
		if err != nil {
			t.Fatal(err)
		}
		// Queue run by inserting a busy host minute, then free capacity before StartQueued.
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
			VALUES ('host-1', $1, 1, 4, 99.0, 8.0, 16000000000, 1000000000)`, *now); err != nil {
			t.Fatal(err)
		}
		runID, err := eng.StartRun(t.Context(), acct, wfID, "Task to cancel")
		if err != nil {
			t.Fatal(err)
		}
		// Make host idle so StartQueued will pick runID.
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
			VALUES ('host-1', $1, 1, 4, 5.0, 0.2, 16000000000, 14000000000)`, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}

		// Hold the run's row lock in a separate transaction so StartQueued completes its
		// unlocked queue scan, picks runID, and blocks on queue.go's FOR UPDATE OF r.
		txCancel, err := d.Owner.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer txCancel.Rollback(t.Context())
		var locked string
		if err := txCancel.QueryRow(t.Context(), `SELECT id::text FROM workflow_runs WHERE id = $1 FOR UPDATE`, runID).Scan(&locked); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			done <- eng.StartQueued(t.Context())
		}()

		// Wait until StartQueued is actively waiting on our row lock in pg_stat_activity.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE OF r%'`).Scan(&waiting)
			if err == nil && waiting > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Cancel runID and commit while StartQueued is blocked right after its unlocked scan.
		if _, err := txCancel.Exec(t.Context(), `UPDATE workflow_runs SET status = $2, finished_at = $3 WHERE id = $1`, runID, Canceled, *now); err != nil {
			t.Fatal(err)
		}
		if err := txCancel.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("StartQueued: %v", err)
		}

		var status string
		if err := d.Owner.QueryRow(t.Context(), `SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != Canceled {
			t.Fatalf("450cced violated: canceled run transitioned to %q during StartQueued", status)
		}
	})

	t.Run("d36a461_concurrent_and_repeat_RetryRun", func(t *testing.T) {
		eng, d, acct, now := setupModelEngine(t)
		wfID, err := eng.CreateWorkflow(t.Context(), acct, "WF", "", modelDef("claude"))
		if err != nil {
			t.Fatal(err)
		}
		runID, err := eng.StartRun(t.Context(), acct, wfID, "Failed task")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status = $2, pending_command = NULL, finished_at = $3 WHERE id = $1`, runID, Failed, *now); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var succeeded int
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := eng.RetryRun(t.Context(), acct, runID); err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if _, err := eng.RetryRun(t.Context(), acct, runID); err == nil {
			succeeded++
		}
		var count int
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_runs WHERE retried_from_run_id = $1`, runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if succeeded != 1 || count != 1 {
			t.Fatalf("d36a461 violated: succeeded=%d retry_rows=%d, want 1", succeeded, count)
		}
	})

	t.Run("016fe16_ask_while_reply_undelivered", func(t *testing.T) {
		eng, d, acct, _ := setupModelEngine(t)
		wfID, err := eng.CreateWorkflow(t.Context(), acct, "WF", "", modelDef("claude"))
		if err != nil {
			t.Fatal(err)
		}
		runID, err := eng.StartRun(t.Context(), acct, wfID, "Ask task")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status = $2, orchestrator_id = 'orch-1', pending_command = NULL WHERE id = $1`, runID, Running); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.MoveRun(t.Context(), "host-1", runID, Move{Move: "ask", Note: "Ship it?"}); err != nil {
			t.Fatalf("first ask: %v", err)
		}
		if err := eng.Reply(t.Context(), acct, runID, "Yes, ship it"); err != nil {
			t.Fatalf("reply: %v", err)
		}
		// Simulate host daemon down: fail all send_message commands carrying the reply.
		if _, err := d.Owner.Exec(t.Context(), `UPDATE commands SET status = 'failed', error = 'daemon down' WHERE kind = 'send_message'`); err != nil {
			t.Fatal(err)
		}
		msg, err := eng.MoveRun(t.Context(), "host-1", runID, Move{Move: "ask", Note: "Ship it?"})
		if err != nil {
			t.Fatalf("second ask: %v", err)
		}
		var status string
		if err := d.Owner.QueryRow(t.Context(), `SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != Running || !strings.Contains(msg, "Yes, ship it") {
			t.Fatalf("016fe16 violated: status=%q msg=%q, want running with undelivered reply", status, msg)
		}
	})

	t.Run("c0d6440_and_149a0a0_stall_epoch_and_budget", func(t *testing.T) {
		eng, d, acct, now := setupModelEngine(t)
		wfID, err := eng.CreateWorkflow(t.Context(), acct, "WF", "", modelDef("claude"))
		if err != nil {
			t.Fatal(err)
		}
		runID, err := eng.StartRun(t.Context(), acct, wfID, "Stall task")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workspaces (host, id, name, directory) VALUES ('host-1', 'ws-1', 'ws', '/w/app')`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO agents (host, id, workspace_id, status, updated_at) VALUES ('host-1', 'orch-1', 'ws-1', 'idle', $1)`, *now); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status = $2, workspace_id = 'ws-1', orchestrator_id = 'orch-1', pending_command = NULL WHERE id = $1`, runID, Running); err != nil {
			t.Fatal(err)
		}

		// Delete lifecycle events to replay c0d6440: watchdog must skip without creating continuations.
		if _, err := d.Owner.Exec(t.Context(), `DELETE FROM workflow_run_events WHERE run_id = $1`, runID); err != nil {
			t.Fatal(err)
		}
		if err := eng.ReconcileStalls(t.Context()); err != nil {
			t.Fatalf("c0d6440 violated: ReconcileStalls without epoch failed: %v", err)
		}
		var conts int
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_run_continuations WHERE run_id = $1`, runID).Scan(&conts); err != nil {
			t.Fatal(err)
		}
		if conts != 0 {
			t.Fatalf("c0d6440 violated: %d continuations without lifecycle epoch", conts)
		}

		// Insert a real lifecycle transition and step time through 2m, 10m, 12m.
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text) VALUES ($1, $2, $3, 'started', 0, 'started')`, runID, acct, *now); err != nil {
			t.Fatal(err)
		}
		advance := func(dt time.Duration) {
			*now = now.Add(dt)
			_, err := d.Owner.Exec(t.Context(), `UPDATE hosts SET last_seen = $1, report_received_at = $1 WHERE name = 'host-1'`, *now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.Owner.Exec(t.Context(), `UPDATE commands SET status = 'succeeded' WHERE kind = 'send_message' AND status IN ('queued','sent','running')`)
			if err != nil {
				t.Fatal(err)
			}
		}
		// Observe idle -> +2m (attempt 1) -> +10m (attempt 2) -> +12m (stalled).
		if err := eng.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
		advance(2 * time.Minute)
		if err := eng.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
		advance(10 * time.Minute)
		if err := eng.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
		advance(12 * time.Minute)
		if err := eng.ReconcileStalls(t.Context()); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := d.Owner.QueryRow(t.Context(), `SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_run_continuations WHERE run_id = $1`, runID).Scan(&conts); err != nil {
			t.Fatal(err)
		}
		if conts != 2 || status != Stalled {
			t.Fatalf("149a0a0 violated: continuations=%d status=%q, want 2 and stalled", conts, status)
		}
	})

	t.Run("2381302_disabled_provider_does_not_block_queue", func(t *testing.T) {
		eng, d, acct, now := setupModelEngine(t)
		wfDisabled, err := eng.CreateWorkflow(t.Context(), acct, "Disabled", "", modelDef("claude"))
		if err != nil {
			t.Fatal(err)
		}
		wfOK, err := eng.CreateWorkflow(t.Context(), acct, "Enabled", "", modelDef("codex"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
			VALUES ('host-1', $1, 1, 4, 99.0, 8.0, 16000000000, 1000000000)`, *now); err != nil {
			t.Fatal(err)
		}
		run0, err := eng.StartRun(t.Context(), acct, wfDisabled, "First")
		if err != nil {
			t.Fatal(err)
		}
		run1, err := eng.StartRun(t.Context(), acct, wfOK, "Second")
		if err != nil {
			t.Fatal(err)
		}
		// Disable claude and free host capacity.
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO provider_plan_settings (provider, enabled) VALUES ('claude', false)
			ON CONFLICT (provider) DO UPDATE SET enabled = false`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
			VALUES ('host-1', $1, 1, 4, 5.0, 0.2, 16000000000, 14000000000)`, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := eng.StartQueued(t.Context()); err != nil {
			t.Fatalf("StartQueued: %v", err)
		}
		var s0, s1 string
		if err := d.Owner.QueryRow(t.Context(), `SELECT status FROM workflow_runs WHERE id = $1`, run0).Scan(&s0); err != nil {
			t.Fatal(err)
		}
		if err := d.Owner.QueryRow(t.Context(), `SELECT status FROM workflow_runs WHERE id = $1`, run1).Scan(&s1); err != nil {
			t.Fatal(err)
		}
		if s0 != Failed || s1 != "starting" {
			t.Fatalf("2381302 violated: run0=%q run1=%q, want failed and starting", s0, s1)
		}
	})
}
