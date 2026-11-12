package workflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Limits are when a host is near capacity: a run queues when the host's projected load would reach
// one of them (room). Zero turns a limit off.
type Limits struct {
	CPUPct     float64 // average CPU busy %
	MemPct     float64 // the share of memory used (by the minute's least available), %
	LoadPerCPU float64 // the 1-minute load average per CPU
	// Fresh is how old the newest minute may be and still say how busy the host is. With an older
	// one, runs wait for a fresh one. Once the newest is Unmonitored old, a host that is connected
	// is treated as one that never sent its resources (its monitoring stopped), whose runs start;
	// one that is away keeps holding until it reports again.
	Fresh, Unmonitored time.Duration
}

// DefaultLimits queue runs at 85% CPU, 90% memory or a load of 1.5 per CPU.
func DefaultLimits() Limits {
	return Limits{CPUPct: 85, MemPct: 90, LoadPerCPU: 1.5, Fresh: 3 * time.Minute, Unmonitored: 30 * time.Minute}
}

// QueueEvery is how often queued runs are checked for room, besides each host minute and each
// run that ends.
const QueueEvery = 15 * time.Second

// lockQueue serializes everything that adds to or starts from host's queue, so its order holds.
func lockQueue(ctx context.Context, tx pgx.Tx, host string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('workflow-queue/' || $1, 0))", host)
	return err
}

// minute is a host's newest minute of resources.
type minute struct {
	At               time.Time
	CPUs             int
	CPU              *float64
	Load1            float64
	MemTotal, MemMin int64
}

// room says why host is too near capacity for another run, or "" if it has room. It projects the
// host's load: its newest minute, plus a run's footprint (host_run_footprint: learned from the
// host's history, or a default; memory uses each run's estimate, repositoryMemory) for each run still
// starting, whose CPU and load that minute cannot show
// (launched since it began, still finding its project, or whose orchestrator the host had not
// started when it began), and for the run to start. A run waits when the projection reaches a
// limit. With no fresh minute it waits for one. A host that never sent its resources gives no sign
// of being near capacity, so it has room; so does one connected now that has sent none for
// Unmonitored (its monitoring stopped). One away that long keeps holding: its runs would all reach
// it at once when it came back. Call it holding the host's queue lock (lockQueue).
func (e *Engine) room(ctx context.Context, tx pgx.Tx, host, candidate, repo string, memory int64) (string, error) {
	// Project selection and retries call room for a run the queue started, while older runs may
	// have gone back into the queue. Honor older queued runs and ready retries on every admission path.
	var ahead int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_run_queue q
		WHERE q.host=$1 AND q.run_id::text<>$2 AND ($2='' OR EXISTS (
			SELECT 1 FROM workflow_runs c WHERE c.id::text=$2 AND (q.queued_at,q.run_id)<(c.created_at,c.id)))`, host, candidate).Scan(&ahead); err != nil {
		return "", err
	}
	if ahead > 0 {
		return fmt.Sprintf("%s queued on %s ahead of it", runsWord(ahead), host), nil
	}
	l, now := e.Limits, e.Now()
	var m minute
	err := tx.QueryRow(ctx, `SELECT minute, cpus, cpu_avg_pct, load1, mem_total, mem_available_min FROM host_minutes
		WHERE host = $1 ORDER BY minute DESC LIMIT 1`, host).Scan(&m.At, &m.CPUs, &m.CPU, &m.Load1, &m.MemTotal, &m.MemMin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if m.At.Before(now.Add(-l.Unmonitored)) {
		// connected: the host holds a stream that was seen in the last minute (as host_status.online)
		var connected bool
		if err := tx.QueryRow(ctx, "SELECT coalesce((SELECT stream_id IS NOT NULL AND last_seen > $2 FROM hosts WHERE name = $1), false)",
			host, now.Add(-time.Minute)).Scan(&connected); err != nil {
			return "", err
		}
		if connected {
			return "", nil
		}
		return fmt.Sprintf("%s has no room yet: waiting for %s to report again (last measured %s)",
			host, host, m.At.UTC().Format(time.RFC3339)), nil
	}
	if m.At.Before(now.Add(-l.Fresh)) {
		return fmt.Sprintf("%s has no room yet: waiting for a fresh measurement from %s (its newest is from %s)",
			host, host, m.At.UTC().Format(time.RFC3339)), nil
	}
	var cpuRun, loadRun float64
	if err := tx.QueryRow(ctx, `SELECT cpu_pct_per_run, load_per_cpu_per_run FROM host_run_footprint WHERE host = $1`,
		host).Scan(&cpuRun, &loadRun); err != nil {
		return "", err
	}
	// starting: runs the newest minute cannot show at full size yet. A run is starting from its launch
	// for as long as it finds its project (its orchestrator is still to come), and then until the host
	// reports its start command (launch_command: the workspace and orchestrator) done and that
	// minute began after it. Memory remains reserved for the whole active run, less
	// its resident memory already included in this exact host minute. With no matching
	// sample, reserve the full estimate. A failed or expired attempt keeps its reservation while
	// its retry is pending (start_retry_at), so the next queued run cannot take the room the retry
	// needs; the retry is sent or the run fails within RetryWaitLimit of its time, so this holds
	// minutes, not hours. A run with no retry left fails, which releases it.
	// A run waiting for Zach's reply reserves nothing more: its agents are idle, perhaps for
	// days, and the memory they do use is already in the minute. Its resident memory is counted
	// apart (waitingMemory) only so the reason can say when sign-offs are what holds the queue.
	var started, active, waiting int
	var reservedMemory, waitingMemory int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE r.status <> $8 AND r.start_retry_at IS NULL AND r.launched_at IS NOT NULL AND
			(r.launched_at >= $2 OR r.status = $5 OR c.id IS NOT NULL AND (c.finished_at IS NULL OR c.finished_at > $2))),
		count(*) FILTER (WHERE r.status <> $8),
		coalesce(sum(greatest(r.estimated_memory_bytes - resident.bytes, 0)) FILTER (WHERE r.status <> $8), 0)::bigint,
		count(*) FILTER (WHERE r.status = $8),
		coalesce(sum(resident.bytes) FILTER (WHERE r.status = $8), 0)::bigint
		FROM workflow_runs r LEFT JOIN commands c ON c.id = r.launch_command
		LEFT JOIN LATERAL (
			SELECT coalesce(sum(m.mem_max), 0)::bigint AS bytes
			FROM (SELECT agent_id FROM workflow_run_agents WHERE run_id = r.id
				UNION SELECT r.orchestrator_id WHERE r.orchestrator_id <> ''
				UNION SELECT r.project_agent_id WHERE r.project_agent_id <> '') members
			JOIN agent_minutes m ON m.host = r.host AND m.agent_id = members.agent_id AND m.minute = $2
		) resident ON true
		WHERE r.host = $1 AND r.status NOT IN ($3, $4, $6)
			AND r.status <> 'queued' AND r.id::text <> $7
			AND (r.start_retry_at IS NOT NULL OR coalesce(c.status NOT IN ('failed','expired'), true))`,
		host, m.At, Failed, Canceled, FindingProject, Succeeded, candidate, Waiting).Scan(&started, &active, &reservedMemory, &waiting, &waitingMemory); err != nil {
		return "", err
	}
	with := "this one"
	if started > 0 {
		with = runsWord(started) + " starting and this one"
	}
	runs := float64(started + 1)
	var why []string
	if l.CPUPct > 0 && m.CPU != nil {
		if p := *m.CPU + runs*cpuRun; p >= l.CPUPct {
			why = append(why, fmt.Sprintf("CPU %.0f%% now, %s with %s (queues at %.0f%%)", *m.CPU, about(p), with, l.CPUPct))
		}
	}
	if l.MemPct > 0 && m.MemTotal > 0 {
		used := 100 * (1 - float64(m.MemMin)/float64(m.MemTotal))
		// The FIFO head starts alone when no other run is active and measured memory is under the
		// limit, whatever its estimate: memory used by other processes could otherwise keep an
		// estimate that fits the host, but not beside them, queued for ever. Keep the full
		// estimate: no following job fits until this one releases its reservation.
		solo := active == 0 && used < l.MemPct
		if p := used + 100*(float64(reservedMemory)+float64(memory))/float64(m.MemTotal); p >= l.MemPct && !solo {
			project := repo
			if project == "" {
				project = "this run"
			}
			memoryWith := "this one"
			if active > 0 {
				memoryWith = runsWord(active) + " active and this one"
			}
			reason := fmt.Sprintf("memory %.0f%% used now, %s with %s (queues at %.0f%%); needs ~%.1f GiB for %s; %.1f GiB free (%.1f GiB reserved for active runs)",
				used, about(p), memoryWith, l.MemPct, float64(memory)/(1<<30), project, float64(m.MemMin)/(1<<30), float64(reservedMemory)/(1<<30))
			// Runs waiting for Zach hold their agents' memory until he signs off, which can take hours.
			// When they hold enough that the run would fit without it, say that plainly.
			if waiting > 0 && waitingMemory > 0 {
				reason += fmt.Sprintf("; %s waiting for sign-off hold %.1f GiB", runsWord(waiting), float64(waitingMemory)/(1<<30))
				if p-100*float64(waitingMemory)/float64(m.MemTotal) < l.MemPct {
					reason += fmt.Sprintf(": waiting for %s to free memory", signOffsWord(waiting))
				}
			}
			why = append(why, reason)
		}
	}
	if l.LoadPerCPU > 0 && m.CPUs > 0 {
		per := m.Load1 / float64(m.CPUs)
		if p := per + runs*loadRun; p >= l.LoadPerCPU {
			why = append(why, fmt.Sprintf("load %.2f per CPU now, about %.2f with %s (queues at %.2f)", per, p, with, l.LoadPerCPU))
		}
	}
	if len(why) == 0 {
		return "", nil
	}
	return host + " is near capacity: " + strings.Join(why, "; "), nil
}

// about reads a projected share: "about 90%", or "over 100%" past all of it.
func about(pct float64) string {
	if pct > 100 {
		return "over 100%"
	}
	return fmt.Sprintf("about %.0f%%", pct)
}

func signOffsWord(n int) string {
	if n == 1 {
		return "a sign-off"
	}
	return fmt.Sprintf("%d sign-offs", n)
}

func runsWord(n int) string {
	if n == 1 {
		return "1 run"
	}
	return fmt.Sprintf("%d runs", n)
}

// StartQueued starts, on each host with a queue, its queued runs in order while the host has room
// (room). Runs started by hand go first, then immediate runs, which start whatever the load.
func (e *Engine) StartQueued(ctx context.Context) error {
	rows, err := e.p.Query(ctx, "SELECT DISTINCT host FROM workflow_run_queue")
	if err != nil {
		return err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, host := range hosts {
		for {
			var started bool
			err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) (err error) { started, err = e.startNext(ctx, tx, host); return err })
			if err != nil {
				errs = append(errs, err)
			}
			if err != nil || !started {
				break
			}
		}
	}
	return errors.Join(errs...)
}

// startNext admits the next run by priority and creation order. Immediate bypasses room.
func (e *Engine) startNext(ctx context.Context, tx pgx.Tx, host string) (bool, error) {
	if err := lockQueue(ctx, tx, host); err != nil {
		return false, err
	}
	// A queued run started by hand comes first: Start now put it ahead of the queue (it may have
	// gone back in when it chose its project, before started_by_hand kept it out). Immediate runs
	// come next, ahead of normal ones.
	r, err := scanRun(tx.QueryRow(ctx, runSelect+`WHERE r.host = $1 AND (r.status = $2 OR r.start_retry_queued)
		ORDER BY (r.status = $2 AND r.started_by_hand) DESC, (r.priority = $3) DESC, r.created_at, r.id LIMIT 1 FOR UPDATE OF r`,
		host, Queued, PriorityImmediate))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.Status == Queued && r.StartedByHand {
		return true, e.dequeue(ctx, tx, r, startedByHand)
	}
	if r.StartRetryAt != nil {
		// Reconciliation records a timeout, and removes a retry whose host is no longer ready.
		if held, err := e.retryHeld(ctx, tx, r); err != nil || held != "" {
			return false, err
		}
	}
	var why string
	if r.Priority != PriorityImmediate {
		why, err = e.room(ctx, tx, host, r.ID, r.Repo, r.EstimatedMemory)
		if err != nil {
			return false, err
		}
	}
	if why != "" {
		_, err := tx.Exec(ctx, "UPDATE workflow_runs SET queue_reason = $2 WHERE id = $1 AND queue_reason <> $2", r.ID, why)
		return false, err
	}
	if r.StartRetryAt != nil {
		return e.dequeueRetry(ctx, tx, r, host+" has room for the retry")
	}
	reason := host + " has room"
	if r.Priority == PriorityImmediate {
		reason = "immediate priority, ahead of the queue"
	}
	return true, e.dequeue(ctx, tx, r, reason)
}

// dequeue starts queued run r, saying why it starts now.
func (e *Engine) dequeue(ctx context.Context, tx pgx.Tx, r run, why string) error {
	if err := e.recordDequeue(ctx, tx, r, why); err != nil {
		return err
	}
	return e.launch(ctx, tx, r)
}

func (e *Engine) recordDequeue(ctx context.Context, tx pgx.Tx, r run, why string) error {
	if err := e.event(ctx, tx, r, "dequeued", nil, nil, why); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE workflow_runs SET queue_reason = '', dequeued_at = $2, updated_at = $2 WHERE id = $1", r.ID, e.Now())
	return err
}

// StartNow starts account's queued run at once, ahead of its queue and whatever its host's load.
// A run whose start retry waits in the queue sends that retry now: it counts as a retry, and a
// failure is handled like any other failed retry.
func (e *Engine) StartNow(ctx context.Context, account int64, runID string) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var host string
		err := tx.QueryRow(ctx, "SELECT host FROM workflow_runs WHERE id = $1 AND account_id = $2", runID, account).Scan(&host)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Take the host's queue lock before the run row, in the same order as startNext.
		if err := lockQueue(ctx, tx, host); err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1 AND r.account_id = $2 FOR UPDATE OF r", runID, account))
		if err != nil {
			return err
		}
		if r.StartRetryAt != nil {
			return e.startRetryNow(ctx, tx, r)
		}
		if r.Status != Queued {
			return conflict("this run is not queued (%s)", strings.ReplaceAll(r.Status, "_", " "))
		}
		// Remembered, so that its project selection and any start retry skip the queue too.
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET started_by_hand = true WHERE id = $1", r.ID); err != nil {
			return err
		}
		r.StartedByHand = true
		return e.dequeue(ctx, tx, r, startedByHand)
	})
}

// retryHeld says why r's start retry cannot be sent yet, whatever its host's load ("" when it can):
// it is still in its backoff, it is RetryWaitLimit overdue (reconciliation sends it with room or
// fails the run), or its host is not ready
// (online, with a connected Paseo daemon that supports retry keys).
func (e *Engine) retryHeld(ctx context.Context, tx pgx.Tx, r run) (string, error) {
	now := e.Now()
	if !now.Before(r.StartRetryAt.Add(RetryWaitLimit)) {
		return "this retry is " + waitLimitWords() + " overdue: it is sent if its host has room, or the run fails", nil
	}
	if now.Before(*r.StartRetryAt) {
		return "this retry is not due until " + r.StartRetryAt.UTC().Format(time.RFC3339), nil
	}
	ready, err := retryHostReady(ctx, tx, r.Host)
	if err != nil || ready {
		return "", err
	}
	return "this retry is waiting for " + r.Host + " to be ready (online, with a connected Paseo daemon that supports retry keys), not for room", nil
}

// startRetryNow sends r's queued start retry at once, whatever its host's load. Only a retry in the
// queue can go: one retryHeld holds waits, as automatic retries do. The caller holds the host's
// queue lock and the run's row.
func (e *Engine) startRetryNow(ctx context.Context, tx pgx.Tx, r run) error {
	held, err := e.retryHeld(ctx, tx, r)
	if err != nil {
		return err
	}
	if held != "" {
		return conflict("%s", held)
	}
	// Remembered, as Start now does: should this attempt fail, its next retry skips the queue too.
	if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET started_by_hand = true WHERE id = $1", r.ID); err != nil {
		return err
	}
	r.StartedByHand = true
	sent, err := e.dequeueRetry(ctx, tx, r, "retry started by hand, ahead of the queue")
	if err != nil {
		return err
	}
	if !sent {
		return conflict("the previous start attempt has just finished; reload the run")
	}
	return nil
}

// startedByHand is the dequeued event's text for Start now. Migration 0043 matches it to mark
// earlier runs started_by_hand.
const startedByHand = "started by hand, ahead of the queue"

// RunQueue starts queued runs as their hosts have room, every interval until ctx ends.
func (e *Engine) RunQueue(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.StartQueued(ctx); err != nil && ctx.Err() == nil {
				log.Printf("workflow queue: %v", err)
			}
		}
	}
}
