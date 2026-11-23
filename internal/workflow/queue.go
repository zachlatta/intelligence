package workflow

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// Limits are when a host is near capacity: a run queues when the host's projected load would reach
// one of them (room). Zero turns a limit off.
type Limits struct {
	CPUPct float64 // average CPU busy %
	// MemPct is the share of memory (used by the minute's least available) the host aims under:
	// what it uses now, with what the runs still starting take as they start.
	MemPct float64
	// MemPeakPct is the share of memory the host may reach should a new run reach its peak (its
	// estimate) while the MemPeaks largest peaks still to come of the runs already going (each
	// one's estimate less what it uses now) come too. A run's peak lasts minutes (a build, a test
	// suite), so the host keeps room for a few at a time, not for every run's at once.
	MemPeakPct float64
	MemPeaks   int
	LoadPerCPU float64 // the 1-minute load average per CPU
	// Fresh is how old the newest minute may be and still say how busy the host is. With an older
	// one, runs wait for a fresh one. Once the newest is Unmonitored old, a host that is connected
	// is treated as one that never sent its resources (its monitoring stopped), whose runs start;
	// one that is away keeps holding until it reports again.
	Fresh, Unmonitored time.Duration
}

// DefaultLimits queue runs at 85% CPU, a load of 1.5 per CPU, 80% memory with the runs still
// starting, or 95% memory should a new run's peak meet the largest peak to come of those going.
func DefaultLimits() Limits {
	return Limits{CPUPct: 85, MemPct: 80, MemPeakPct: 95, MemPeaks: 1, LoadPerCPU: 1.5, Fresh: 3 * time.Minute, Unmonitored: 30 * time.Minute}
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

// hostRun is one of a host's runs as its capacity sees it.
type hostRun struct {
	ID      string
	Created time.Time
	// Active runs take room: admitted, not waiting for Zach, not finished, and not on a failed or
	// expired launch (unless its retry is pending, which keeps the room it needs).
	Active, Waiting bool
	// Starting: the newest minute cannot show its load yet; Reserving: starting, or a start retry
	// is pending, so its start still takes memory.
	Starting, Reserving bool
	Estimate, Resident  int64 // its memory estimate, and what its agents used in the newest minute
}

// capacity is what a host holds, read once per decision while holding its queue lock: its newest
// minute and its runs.
type capacity struct {
	host   string
	limits Limits
	// hold is why every run waits whatever its size (no fresh measurement); open says there is no
	// measurement to judge by (a host that never sent one, or connected but no longer sending),
	// so every run has room.
	hold            string
	open            bool
	m               minute
	cpuRun, loadRun float64
	runs            []hostRun
}

// loadCapacity reads host's capacity (see room).
func (e *Engine) loadCapacity(ctx context.Context, tx pgx.Tx, host string) (*capacity, error) {
	c := &capacity{host: host, limits: e.Limits}
	l, now := e.Limits, e.Now()
	err := tx.QueryRow(ctx, `SELECT minute, cpus, cpu_avg_pct, load1, mem_total, mem_available_min FROM host_minutes
		WHERE host = $1 ORDER BY minute DESC LIMIT 1`, host).Scan(&c.m.At, &c.m.CPUs, &c.m.CPU, &c.m.Load1, &c.m.MemTotal, &c.m.MemMin)
	if errors.Is(err, pgx.ErrNoRows) {
		c.open = true
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if c.m.At.Before(now.Add(-l.Unmonitored)) {
		// connected: the host holds a stream that was seen in the last minute (as host_status.online)
		var connected bool
		if err := tx.QueryRow(ctx, "SELECT coalesce((SELECT stream_id IS NOT NULL AND last_seen > $2 FROM hosts WHERE name = $1), false)",
			host, now.Add(-time.Minute)).Scan(&connected); err != nil {
			return nil, err
		}
		if connected {
			c.open = true
			return c, nil
		}
		c.hold = fmt.Sprintf("%s has no room yet: waiting for %s to report again (last measured %s)",
			host, host, c.m.At.UTC().Format(time.RFC3339))
		return c, nil
	}
	if c.m.At.Before(now.Add(-l.Fresh)) {
		c.hold = fmt.Sprintf("%s has no room yet: waiting for a fresh measurement from %s (its newest is from %s)",
			host, host, c.m.At.UTC().Format(time.RFC3339))
		return c, nil
	}
	if err := tx.QueryRow(ctx, `SELECT cpu_pct_per_run, load_per_cpu_per_run FROM host_run_footprint WHERE host = $1`,
		host).Scan(&c.cpuRun, &c.loadRun); err != nil {
		return nil, err
	}
	// Starting: from a run's launch for as long as it finds its project (its orchestrator is still
	// to come), and then until the host reports its start command (launch_command: the workspace and
	// orchestrator) done and that minute began after it. A failed or expired attempt keeps its room
	// while its retry is pending (start_retry_at), so the next queued run cannot take the room the
	// retry needs; the retry is sent or the run fails within RetryWaitLimit of its time, so this
	// holds minutes, not hours. A run with no retry left fails, which releases it.
	// Resident: its agents' memory in this exact minute. Runs that ended since the minute began
	// are read too: the minute still shows their memory.
	rows, err := tx.Query(ctx, `SELECT r.id::text, r.created_at,
			r.status NOT IN ($3, $4, $5, $6, $7) AND (r.start_retry_at IS NOT NULL OR coalesce(c.status NOT IN ('failed','expired'), true)),
			r.status = $6,
			r.start_retry_at IS NULL AND r.launched_at IS NOT NULL AND
				(r.launched_at >= $2 OR r.status = $8 OR c.id IS NOT NULL AND (c.finished_at IS NULL OR c.finished_at > $2)),
			r.start_retry_at IS NOT NULL,
			r.estimated_memory_bytes, resident.bytes
		FROM workflow_runs r LEFT JOIN commands c ON c.id = r.launch_command
		LEFT JOIN LATERAL (
			SELECT coalesce(sum(m.mem_max), 0)::bigint AS bytes
			FROM (SELECT agent_id FROM workflow_run_agents WHERE run_id = r.id
				UNION SELECT r.orchestrator_id WHERE r.orchestrator_id <> ''
				UNION SELECT r.project_agent_id WHERE r.project_agent_id <> '') members
			JOIN agent_minutes m ON m.host = r.host AND m.agent_id = members.agent_id AND m.minute = $2
		) resident ON true
		WHERE r.host = $1 AND (r.status NOT IN ($3, $4, $5) OR r.finished_at >= $2)
		ORDER BY r.created_at, r.id`,
		host, c.m.At, Failed, Canceled, Succeeded, Waiting, Queued, FindingProject)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r hostRun
		var retry bool
		if err := rows.Scan(&r.ID, &r.Created, &r.Active, &r.Waiting, &r.Starting, &retry, &r.Estimate, &r.Resident); err != nil {
			rows.Close()
			return nil, err
		}
		r.Starting = r.Starting && r.Active
		r.Reserving = r.Active && (r.Starting || retry)
		c.runs = append(c.runs, r)
	}
	return c, rows.Err()
}

// candidate is a run asking for room: its id ("" for one not made yet), when it was made (its
// place in the queue), its memory estimate and its repository.
type candidate struct {
	ID      string
	Created time.Time
	Memory  int64
	Repo    string
}

// fits says why c's host has no room for k, or "" if it has. With alone, it counts only the runs
// that came before k (made before it): whether the runs after it are all that keep it waiting.
//
// It projects the host from its newest minute. CPU and load: the minute's, plus a run's footprint
// (host_run_footprint: learned from the host's history, or a default) for each run still starting,
// whose load the minute cannot show, and for k. Memory: the minute's use, plus what each run still
// starting takes as it starts (its estimate less what it uses now, up to finderMemory, what a
// finder or orchestrator takes), and the same for k; this aims under MemPct. Each run may yet
// reach its peak (its estimate, less what it uses now and what its start counts): k must have room
// for its own peak while the MemPeaks largest peaks of the runs going come too, under MemPeakPct.
// So two runs whose peaks cannot both fit do not start together, but lighter runs fit beside a
// heavy one. A run's peak is a build or a test suite of a few minutes, and the host's history
// (2026-11) has two runs at their peaks at once for 6 minutes in 3 days and never three; reserving
// every run's peak at once held a 19 GiB host full at 30% used.
//
// Alone may pass the steady ceiling only with an estimate at most half the host
// and its entire unconsumed peak below the peak ceiling. An oversized head still
// permits backfill; it cannot claim the unbounded solo exception.
func (c *capacity) fits(k candidate, alone bool) string {
	if c.open {
		return ""
	}
	if c.hold != "" {
		return c.hold
	}
	l, m := c.limits, c.m
	before := func(r hostRun) bool {
		return r.Created.Before(k.Created) || r.Created.Equal(k.Created) && r.ID < k.ID
	}
	var started, active, waiting int
	var usedBytes, waitingBytes, starts, ownResident float64
	var peaks []float64 // of the runs going
	usedBytes = float64(m.MemTotal - m.MemMin)
	for _, r := range c.runs {
		if r.ID == k.ID {
			ownResident = float64(r.Resident)
			continue
		}
		if alone && k.ID != "" && !before(r) {
			usedBytes -= float64(r.Resident) // the minute's memory of a run after k
			continue
		}
		if r.Waiting {
			waiting++
			waitingBytes += float64(r.Resident)
		}
		if !r.Active {
			continue
		}
		active++
		if r.Starting {
			started++
		}
		headroom := float64(max(r.Estimate-r.Resident, 0))
		if r.Reserving {
			start := min(headroom, float64(finderMemory))
			starts += start
			headroom -= start
		}
		peaks = append(peaks, headroom)
	}
	kHeadroom := max(float64(k.Memory)-ownResident, 0)
	kStart := min(kHeadroom, float64(finderMemory))
	starts += kStart
	with := "this one"
	if started > 0 {
		with = runsWord(started) + " starting and this one"
	}
	runs := float64(started + 1)
	var why []string
	if l.CPUPct > 0 && m.CPU != nil {
		if p := *m.CPU + runs*c.cpuRun; p >= l.CPUPct {
			why = append(why, fmt.Sprintf("CPU %.0f%% now, %s with %s (queues at %.0f%%)", *m.CPU, about(p), with, l.CPUPct))
		}
	}
	if m.MemTotal > 0 {
		pct := func(b float64) float64 { return 100 * b / float64(m.MemTotal) }
		used := pct(max(usedBytes, 0))
		solo := active == 0 && k.Memory <= m.MemTotal/2 &&
			(l.MemPct <= 0 || used < l.MemPct) &&
			(l.MemPeakPct <= 0 || used+pct(kHeadroom) < l.MemPeakPct)
		slices.SortFunc(peaks, func(a, b float64) int { return cmp.Compare(b, a) })
		counted := peaks[:min(max(l.MemPeaks, 0), len(peaks))]
		var othersPeak float64
		for _, p := range counted {
			othersPeak += p
		}
		peak := kHeadroom - kStart + othersPeak
		steady := used + pct(starts)
		var reason string
		var over, limit float64
		switch {
		case solo:
		case l.MemPct > 0 && steady >= l.MemPct:
			over, limit = steady, l.MemPct
			reason = fmt.Sprintf("memory %.0f%% used now, %s with %s (queues at %.0f%%)", used, about(steady), with, l.MemPct)
		case l.MemPeakPct > 0 && steady+pct(peak) >= l.MemPeakPct:
			over, limit = steady+pct(peak), l.MemPeakPct
			reason = fmt.Sprintf("memory %.0f%% used now, %s %s (queues at %.0f%%)", used, about(over), peaksWords(len(counted)), l.MemPeakPct)
		}
		if reason != "" {
			project := k.Repo
			if project == "" {
				project = "this run"
			}
			reason += fmt.Sprintf("; needs ~%.1f GiB for %s; %.1f GiB free (%.1f GiB held for active runs' starts and peaks)",
				float64(k.Memory)/(1<<30), project, float64(m.MemMin)/(1<<30), (starts-kStart+othersPeak)/(1<<30))
			// Runs waiting for Zach hold their agents' memory until he signs off, which can take hours.
			// When they hold enough that the run would fit without it, say that plainly.
			if waiting > 0 && waitingBytes > 0 {
				reason += fmt.Sprintf("; %s waiting for sign-off hold %.1f GiB", runsWord(waiting), waitingBytes/(1<<30))
				if over-pct(waitingBytes) < limit {
					reason += fmt.Sprintf(": waiting for %s to free memory", signOffsWord(waiting))
				}
			}
			why = append(why, reason)
		}
	}
	if l.LoadPerCPU > 0 && m.CPUs > 0 {
		per := m.Load1 / float64(m.CPUs)
		if p := per + runs*c.loadRun; p >= l.LoadPerCPU {
			why = append(why, fmt.Sprintf("load %.2f per CPU now, about %.2f with %s (queues at %.2f)", per, p, with, l.LoadPerCPU))
		}
	}
	if len(why) == 0 {
		return ""
	}
	return c.host + " is near capacity: " + strings.Join(why, "; ")
}

// peaksWords says which peaks the projection counts: the new run's, and n of the runs going.
func peaksWords(n int) string {
	switch n {
	case 0:
		return "at its peak"
	case 1:
		return "should its peak meet the largest to come of the runs going"
	}
	return fmt.Sprintf("should its peak meet the %d largest to come of the runs going", n)
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

// queuedRuns are host's queued runs and ready retries, in the order the queue starts them: runs
// started by hand, then immediate ones, then by when they were made.
func queuedRuns(ctx context.Context, tx pgx.Tx, host string) ([]run, []time.Time, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, created_at FROM workflow_runs r WHERE r.host = $1 AND (r.status = $2 OR r.start_retry_queued)
		ORDER BY (r.status = $2 AND r.started_by_hand) DESC, (r.priority = $3) DESC, r.created_at, r.id`, host, Queued, PriorityImmediate)
	if err != nil {
		return nil, nil, err
	}
	type entry struct {
		id string
		at time.Time
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (entry, error) {
		var e entry
		return e, row.Scan(&e.id, &e.at)
	})
	if err != nil {
		return nil, nil, err
	}
	runs, created := make([]run, 0, len(entries)), make([]time.Time, 0, len(entries))
	for _, en := range entries {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1", en.id))
		if err != nil {
			return nil, nil, err
		}
		runs, created = append(runs, r), append(created, en.at)
	}
	return runs, created, nil
}

// heldFor is why a run behind queued run q waits for it: q would have room were it not for the
// runs that came after it, so the queue holds for it rather than let later runs keep it waiting.
func heldFor(ahead int, host string, q run) string {
	return fmt.Sprintf("%s queued on %s ahead of it; the queue holds for %q, which needs the room runs started after it are using",
		runsWord(ahead), host, q.Title)
}

// room says why a run (candidate's id, "" for a new one; its repository and memory estimate) has
// to wait on host, or "" if it can start now (fits). A run waits for the runs queued before it
// that can start now, and for one the runs after it are keeping waiting (heldFor). Runs ahead that
// older runs are holding (they need more room than the host has beside those) do not hold it: a
// run that fits starts ahead of them, so a heavy run at the head no longer keeps every light run
// behind it waiting. Call it holding the host's queue lock (lockQueue).
func (e *Engine) room(ctx context.Context, tx pgx.Tx, host, candidateID, repo string, memory int64) (string, error) {
	c, err := e.loadCapacity(ctx, tx, host)
	if err != nil {
		return "", err
	}
	k := candidate{ID: candidateID, Created: e.Now(), Memory: memory, Repo: repo}
	if candidateID != "" {
		if err := tx.QueryRow(ctx, "SELECT created_at FROM workflow_runs WHERE id::text = $1", candidateID).Scan(&k.Created); err != nil {
			return "", err
		}
	}
	queue, created, err := queuedRuns(ctx, tx, host)
	if err != nil {
		return "", err
	}
	var ahead []int
	for i, q := range queue {
		if q.ID != candidateID && (q.skipsQueue() || created[i].Before(k.Created) || created[i].Equal(k.Created) && q.ID < k.ID) {
			ahead = append(ahead, i)
		}
	}
	for _, i := range ahead {
		q := queue[i]
		if q.skipsQueue() {
			return fmt.Sprintf("%s queued on %s ahead of it", runsWord(len(ahead)), host), nil
		}
		if q.StartRetryAt != nil {
			if held, err := e.retryHeld(ctx, tx, q); err != nil || held != "" {
				if err != nil {
					return "", err
				}
				continue // a retry held for its backoff or its host does not hold the queue
			}
		}
		qk := candidate{ID: q.ID, Created: created[i], Memory: q.EstimatedMemory, Repo: q.Repo}
		if c.fits(qk, false) == "" {
			return fmt.Sprintf("%s queued on %s ahead of it", runsWord(len(ahead)), host), nil
		}
		if c.fits(qk, true) == "" {
			return heldFor(len(ahead), host, q), nil
		}
	}
	return c.fits(k, false), nil
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
			txCtx, flushPushes := e.CollectPushes(ctx)
			var started bool
			err := pgx.BeginFunc(txCtx, e.p, func(tx pgx.Tx) (err error) { started, err = e.startNext(txCtx, tx, host); return err })
			if err != nil {
				errs = append(errs, err)
			} else {
				flushPushes()
			}
			if err != nil || !started {
				break
			}
		}
	}
	return errors.Join(errs...)
}

// startNext starts the first of host's queued runs that can start (room), in the queue's order,
// and says whether it started one. A run started by hand or with immediate priority starts
// whatever the load. Every run it passes over is told why it waits (queue_reason).
func (e *Engine) startNext(ctx context.Context, tx pgx.Tx, host string) (bool, error) {
	if err := lockQueue(ctx, tx, host); err != nil {
		return false, err
	}
	queue, created, err := queuedRuns(ctx, tx, host)
	if err != nil || len(queue) == 0 {
		return false, err
	}
	c, err := e.loadCapacity(ctx, tx, host)
	if err != nil {
		return false, err
	}
	reasons := map[string]string{}
	var pick *run
	var why string
	var holding *run // a run ahead that only the runs after it keep waiting
	passed := 0
	for i := range queue {
		r := queue[i]
		// A queued run started by hand comes first: Start now put it ahead of the queue (it may have
		// gone back in when it chose its project, before started_by_hand kept it out). Immediate runs
		// come next, ahead of normal ones.
		if r.Status == Queued && r.StartedByHand {
			pick, why = &r, startedByHand
			break
		}
		if r.StartRetryAt != nil {
			// Reconciliation records a timeout, and removes a retry whose host is no longer ready.
			held, err := e.retryHeld(ctx, tx, r)
			if err != nil {
				return false, err
			}
			if held != "" {
				continue
			}
		}
		if r.Priority == PriorityImmediate {
			pick, why = &r, "immediate priority, ahead of the queue"
			break
		}
		if holding != nil {
			reasons[r.ID] = heldFor(i, host, *holding)
			continue
		}
		k := candidate{ID: r.ID, Created: created[i], Memory: r.EstimatedMemory, Repo: r.Repo}
		if reason := c.fits(k, false); reason != "" {
			reasons[r.ID] = reason
			passed++
			if c.fits(k, true) == "" {
				holding = &queue[i]
			}
			continue
		}
		pick, why = &r, host+" has room"
		if passed > 0 {
			why += fmt.Sprintf(", ahead of %s queued before it that need more room", runsWord(passed))
		}
		break
	}
	for id, reason := range reasons {
		if _, err := tx.Exec(ctx, "UPDATE workflow_runs SET queue_reason = $2 WHERE id = $1 AND queue_reason <> $2", id, reason); err != nil {
			return false, err
		}
	}
	if pick == nil {
		return false, nil
	}
	// The queue was read without locking its runs: lock the one picked only if it is still in the
	// queue (Postgres checks again after waiting for its row), so a run canceled meanwhile is not
	// started. One that left the queue is progress: the next pass reads the queue again.
	r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id = $1 AND (r.status = $2 OR r.start_retry_queued) FOR UPDATE OF r",
		pick.ID, Queued))
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if r.Status == Queued && r.StartedByHand {
		return true, e.dequeue(ctx, tx, r, why)
	}
	// A permanent refusal must not leave this run blocking the host.
	// Roll back any partial launch (including model allocations and commands)
	// before recording its terminal failure in the outer transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	spCtx, commitPushes := e.CollectPushes(ctx)
	var started bool
	if r.StartRetryAt != nil {
		started, err = e.dequeueRetry(spCtx, sp, r, host+" has room for the retry")
	} else {
		err = e.dequeue(spCtx, sp, r, why)
		started = err == nil
	}
	if err == nil {
		if err := sp.Commit(ctx); err != nil {
			return false, err
		}
		commitPushes()
		return started, nil
	}
	if rollbackErr := sp.Rollback(ctx); rollbackErr != nil {
		return false, rollbackErr
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return false, err
	}
	if err := e.event(ctx, tx, r, "error", nil, nil, refusal.Error()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET status=$2,error=$3,pending_command=NULL,
		start_retry_at=NULL,start_retry_queued=false,queue_reason='',updated_at=$4,finished_at=$4 WHERE id=$1`,
		r.ID, Failed, refusal.Error(), e.Now()); err != nil {
		return false, err
	}
	if err := e.recordFailureAlert(ctx, tx, r, refusal.Error()); err != nil {
		return false, err
	}
	return true, nil // progressed: continue with the next queued run
}

// dequeue starts queued run r, saying why it starts now.
func (e *Engine) dequeue(ctx context.Context, tx pgx.Tx, r run, why string) error {
	if err := e.recordDequeue(ctx, tx, r, why); err != nil {
		return err
	}
	return e.launch(ctx, tx, r)
}

// startAttempt rolls back partial startup writes (including model allocations) in a
// savepoint. A disabled subscription ends this run in the outer transaction, freeing
// its place in the queue. Other errors still roll back the caller's transaction.
func (e *Engine) startAttempt(ctx context.Context, tx pgx.Tx, r run, attempt func(pgx.Tx) error) (bool, error) {
	err := pgx.BeginFunc(ctx, tx, attempt)
	var disabled *fleet.DisabledProviderError
	if !errors.As(err, &disabled) {
		return false, err
	}
	if err := e.event(ctx, tx, r, "error", nil, nil, disabled.Error()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET status=$2, error=$3, pending_command=NULL,
		start_retry_at=NULL, start_retry_queued=false, queue_reason='', updated_at=$4, finished_at=$4
		WHERE id=$1`, r.ID, Failed, disabled.Error(), e.Now()); err != nil {
		return false, err
	}
	if err := e.recordFailureAlert(ctx, tx, r, disabled.Error()); err != nil {
		return false, err
	}
	return true, e.archiveRun(ctx, tx, r)
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
	ctx, flushPushes := e.CollectPushes(ctx)
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
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
	if err == nil {
		e.sendFailureAlertsBestEffort(ctx)
		flushPushes()
	}
	return err
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
