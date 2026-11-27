package hostclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/resources"
)

// Client is the host client: it follows the host's Paseo daemon (Watcher), holds its stream to the
// server (Link), and over it reports the daemon, copies its agents' transcripts, sends its plans'
// usage and runs the server's commands. Each runs on its own, so a slow transcript upload holds up no report.
type Client struct {
	Name    string // the host's name in the fleet
	Version string // this binary's version
	Watcher *Watcher
	Link    *Link // Run sets its ServerVersion when Update is set
	// Update moves the host to the server's version (Updater.Maybe), called with the version from
	// every dial; nil keeps this binary.
	Update func(ctx context.Context, serverVersion string) error
	// Usage reads the host's plans' usage and sends it up the stream (Run sets its Send); nil sends none.
	Usage *UsageSync
	// Resources samples the host and its agents' processes and sends each minute up the stream (Run
	// sets its Send, and its Reader's PaseoPID from the Watcher); nil sends none.
	Resources *resources.Collector
	// SwitchLogin makes login the one the host's provider tool uses (switch_login) and returns its
	// slot; nil refuses the command.
	SwitchLogin func(ctx context.Context, provider, login string) (int, error)

	ReportEvery time.Duration // periodic full snapshot; default 30s
	StatusEvery time.Duration // re-read the daemon's status (a change is reported); default 15s
	LiveEvery   time.Duration // copy running agents' transcripts; default 2s
	// A live agent's transcript (LiveLabel) is copied on each push of its timeline from the
	// daemon: a LiveFast one at most every FastGap (default 100ms), others every SteadyGap
	// (default 1s), at most SteadyBudget (default 8) of those a second together.
	FastGap      time.Duration
	SteadyGap    time.Duration
	SteadyBudget int
	DiffEvery    time.Duration // re-read the diffs of workspaces an agent works in; default 5s
	Settle       time.Duration // gather a burst of daemon updates into one report; default 1s
	Logf         func(format string, args ...any)

	schedules       *ScheduleSync
	scheduleMu      sync.Mutex
	lastScheduleErr string           // syncSchedules logs an error once
	reportTicks     <-chan time.Time // nil uses ReportEvery; tests can advance reporting explicitly
}

// Run runs the host until ctx ends.
func (b *Client) Run(ctx context.Context) error {
	if b.ReportEvery == 0 {
		b.ReportEvery = 30 * time.Second
	}
	if b.StatusEvery == 0 {
		b.StatusEvery = 15 * time.Second
	}
	if b.LiveEvery == 0 {
		b.LiveEvery = 2 * time.Second
	}
	if b.FastGap == 0 {
		b.FastGap = 100 * time.Millisecond
	}
	if b.SteadyGap == 0 {
		b.SteadyGap = time.Second
	}
	if b.SteadyBudget == 0 {
		b.SteadyBudget = 8
	}
	if b.DiffEvery == 0 {
		b.DiffEvery = 5 * time.Second
	}
	if b.Settle == 0 {
		b.Settle = time.Second
	}
	if b.Logf == nil {
		b.Logf = log.Printf
	}
	versions := make(chan string, 1)
	if b.Update != nil {
		b.Link.ServerVersion = func(v string) {
			select {
			case versions <- v:
			default:
			}
		}
	}
	reported := &latest{wake: make(chan struct{}, 1)}
	diffed := &latest{wake: make(chan struct{}, 1)}
	b.schedules = &ScheduleSync{List: b.Watcher.Schedules, Inspect: b.Watcher.InspectSchedule, Send: b.Link.Schedules}
	runs := []func(context.Context){
		b.Watcher.Run,
		b.Link.Run,
		b.runCommands,
		b.watchPermissions,
		func(ctx context.Context) { b.syncTranscripts(ctx, reported) },
		func(ctx context.Context) { b.syncDiffs(ctx, diffed) },
		func(ctx context.Context) { b.update(ctx, versions) },
	}
	if b.Usage != nil {
		b.Usage.Send = b.Link.Usage
		runs = append(runs, b.Usage.Run)
	}
	if b.Resources != nil {
		b.Resources.Send = b.Link.Resources
		if b.Resources.Reader != nil && b.Resources.Reader.PaseoPID == nil {
			b.Resources.Reader.PaseoPID = func() int { return b.Watcher.Snapshot().PID }
		}
		runs = append(runs, b.Resources.Run)
	}
	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Go(func() { run(ctx) })
	}
	b.report(ctx, reported, diffed)
	wg.Wait()
	return nil
}

// report sends a report when the host connects (a new stream: the server may have missed
// changes) and when the daemon's state changes, and hands each stored one to transcript syncing.
// A report that fails is sent again on the next status read or connection.
func (b *Client) report(ctx context.Context, reported ...*latest) {
	status := time.NewTicker(b.StatusEvery)
	defer status.Stop()
	heartbeat := b.reportTicks
	if heartbeat == nil {
		ticker := time.NewTicker(b.ReportEvery)
		defer ticker.Stop()
		heartbeat = ticker.C
	}
	due, fresh := false, false // a report is due; the next one is the first on a new stream
	lastDaemon, lastErr := "", ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.Link.Connected():
			b.Logf("stream to %s: connected", b.Link.Server)
			due, fresh = true, true
			b.schedules.Reset() // the server may have missed changes
		case <-b.Watcher.Changed():
			select { // a burst of updates (an agent's turn starting, its workspace moving) goes in one report
			case <-ctx.Done():
				return
			case <-time.After(b.Settle):
			}
			due = true
		case <-heartbeat:
			due = true
		case <-status.C:
			b.Watcher.RefreshStatus(ctx) // a change comes back through Changed
			b.syncSchedules(ctx)
		}
		snap := b.Watcher.Snapshot()
		if snap.Error != lastDaemon {
			if snap.Error != "" {
				b.Logf("paseo daemon: %s", snap.Error)
			} else {
				b.Logf("paseo daemon: connected")
			}
			lastDaemon = snap.Error
		}
		if !due {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := b.Link.Report(sendCtx, Report{Name: b.Name, Version: b.Version, Paseo: snap, CollectedAt: time.Now(), StartRetryKeys: true})
		cancel()
		if err != nil {
			if msg := err.Error(); msg != lastErr && ctx.Err() == nil {
				b.Logf("report failed: %v (stream: %s)", err, b.Link.Err())
				lastErr = msg
			}
			continue
		}
		if lastErr != "" {
			b.Logf("reporting again")
			lastErr = ""
		}
		for _, l := range reported {
			l.put(snap, fresh)
		}
		if fresh {
			b.syncSchedules(ctx)
		}
		due, fresh = false, false
	}
}

// syncSchedules sends the daemon's schedules if they changed. The daemon pushes nothing when a
// schedule changes, so this runs with every status read, on each new stream, and after each
// schedule command.
func (b *Client) syncSchedules(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := b.schedules.Sync(sctx)
	b.scheduleMu.Lock()
	defer b.scheduleMu.Unlock()
	if msg := fmt.Sprint(err); err != nil && msg != b.lastScheduleErr && !errors.Is(err, ErrNotConnected) && ctx.Err() == nil {
		b.Logf("schedules: %v", err)
	}
	b.lastScheduleErr = fmt.Sprint(err)
}

// syncTranscripts copies loaded agents' transcripts after each stored report (once the server
// knows the agents) and, while one is running, every LiveEvery. A live agent's (LiveLabel) is
// copied as the daemon pushes each change to its timeline, paced by livePace, so a page that
// streams it sees its words as they come.
func (b *Client) syncTranscripts(ctx context.Context, reported *latest) {
	ts := &TranscriptSync{Host: b.Name, Timeline: b.Watcher.Timeline, Cursors: b.Link.TranscriptCursors, Send: b.Link.Transcript}
	live := time.NewTicker(b.LiveEvery)
	defer live.Stop()
	pace := &livePace{fast: b.FastGap, steady: b.SteadyGap, budget: b.SteadyBudget, last: map[string]time.Time{}, dirty: map[string]bool{}}
	var due *time.Timer
	var dueC <-chan time.Time
	var snap PaseoStatus
	stored := false // a report has been stored on this stream, so the server knows the agents
	lastErr := ""
	for {
		var agents []Agent
		select {
		case <-ctx.Done():
			return
		case <-reported.wake:
			var fresh bool
			snap, fresh = reported.take()
			if fresh {
				ts.Reset() // the server's copies may have changed while the stream was down
			}
			stored = true
			agents = snap.Agents
		case <-live.C:
			if !anyRunning(snap.Agents) {
				continue
			}
			snap = b.Watcher.Snapshot()
			agents = snap.Agents
		case id := <-b.Watcher.TimelineUpdates():
			pace.dirty[id] = true
		case <-dueC:
			dueC = nil
		}
		// every push and every due time takes what is ready now (a fast agent's push is never
		// held behind a steady one's wait), and re-arms the timer for the next to come due
		if stored && len(pace.dirty) > 0 {
			snap = b.Watcher.Snapshot()
			ready, wait := pace.take(snap.Agents, time.Now())
			agents = append(agents, ready...)
			if due != nil && dueC != nil && !due.Stop() {
				<-due.C
			}
			dueC = nil
			if wait > 0 {
				if due == nil {
					due = time.NewTimer(wait)
				} else {
					due.Reset(wait)
				}
				dueC = due.C
			}
		}
		if !snap.Connected || len(agents) == 0 {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := ts.Sync(syncCtx, agents)
		cancel()
		if msg := fmt.Sprint(err); err != nil && msg != lastErr && ctx.Err() == nil {
			b.Logf("transcript sync: %v", err)
			lastErr = msg
		} else if err == nil {
			lastErr = ""
		}
	}
}

// livePace decides which pushed agents to copy now: a LiveFast agent once FastGap has passed
// since its last copy, another once SteadyGap has, and the steady ones at most budget a second
// together. The rest wait; take says how long until the next is due.
type livePace struct {
	fast, steady time.Duration
	budget       int
	last         map[string]time.Time
	dirty        map[string]bool
	window       time.Time // the current second of the steady budget
	spent        int
}

func (p *livePace) take(agents []Agent, now time.Time) ([]Agent, time.Duration) {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	if now.Sub(p.window) >= time.Second {
		p.window, p.spent = now, 0
	}
	var ready []Agent
	wait := time.Duration(0)
	later := func(d time.Duration) {
		if wait == 0 || d < wait {
			wait = d
		}
	}
	for id := range p.dirty {
		a, ok := byID[id]
		if !ok || a.Live == "" {
			delete(p.dirty, id)
			continue
		}
		gap := p.steady
		if a.Live == LiveFast {
			gap = p.fast
		}
		if left := gap - now.Sub(p.last[id]); left > 0 {
			later(left)
			continue
		}
		if a.Live != LiveFast {
			if p.spent >= p.budget {
				later(time.Second - now.Sub(p.window))
				continue
			}
			p.spent++
		}
		delete(p.dirty, id)
		p.last[id] = now
		ready = append(ready, a)
	}
	return ready, wait
}

// syncDiffs sends the git workspaces' diffs that changed after each stored report and, while an
// agent works, every DiffEvery.
func (b *Client) syncDiffs(ctx context.Context, reported *latest) {
	ds := &DiffSync{Read: b.Watcher.CheckoutDiff, Send: b.Link.Diff}
	tick := time.NewTicker(b.DiffEvery)
	defer tick.Stop()
	lastErr := ""
	for {
		var snap PaseoStatus
		select {
		case <-ctx.Done():
			return
		case <-reported.wake:
			var fresh bool
			snap, fresh = reported.take()
			if fresh {
				ds.Reset() // the server may have lost them while the stream was down
			}
		case <-tick.C:
			if snap = b.Watcher.Snapshot(); !anyRunning(snap.Agents) {
				continue
			}
		}
		syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := ds.Sync(syncCtx, snap)
		cancel()
		if msg := fmt.Sprint(err); err != nil && msg != lastErr && !errors.Is(err, ErrNotConnected) &&
			!errors.Is(err, errNoDaemon) && ctx.Err() == nil {
			b.Logf("diff sync: %v", err)
			lastErr = msg
		} else if err == nil {
			lastErr = ""
		}
	}
}

func (b *Client) runCommands(ctx context.Context) {
	exec := func(ctx context.Context, cmd Command) (CommandResult, error) {
		if cmd.Kind == SwitchLoginKind {
			return b.switchLogin(ctx, cmd)
		}
		res, err := b.Watcher.Execute(ctx, cmd)
		if IsScheduleKind(cmd.Kind) {
			b.syncSchedules(ctx) // so the server has the change before the command's result
		}
		return res, err
	}
	runner := &Runner{Exec: exec, Report: b.Link.Update}
	defer runner.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-b.Link.Commands():
			b.Logf("command %s: %s", cmd.ID, cmd.Kind)
			runner.Handle(ctx, cmd)
		}
	}
}

// switchLogin runs switch_login, then sends a fresh usage read before the result, so the server
// knows the new active login by the time it hears the switch succeeded.
func (b *Client) switchLogin(ctx context.Context, cmd Command) (CommandResult, error) {
	parsed, err := ParseCommand(cmd.Kind, cmd.Args)
	if err != nil {
		return CommandResult{}, err
	}
	a := parsed.(SwitchLoginArgs)
	if b.SwitchLogin == nil {
		return CommandResult{}, errors.New("this host cannot switch logins")
	}
	slot, err := b.SwitchLogin(ctx, a.Provider, a.Login)
	if err != nil {
		return CommandResult{Slot: slot}, err
	}
	if b.Usage != nil && b.Usage.Read != nil && b.Usage.Send != nil {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := b.Usage.Send(sendCtx, b.Usage.Read(sendCtx)); err != nil {
			b.Logf("usage after switching login: %v", err)
		}
		cancel()
	}
	return CommandResult{Login: a.Login, Slot: slot}, nil
}

// update follows the server's version.
func (b *Client) update(ctx context.Context, versions <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-versions:
			if err := b.Update(ctx, v); err != nil {
				b.Logf("self-update failed: %v", err)
			}
		}
	}
}

func anyRunning(agents []Agent) bool {
	for _, a := range agents {
		if a.Status == "running" {
			return true
		}
	}
	return false
}

// latest holds the last stored report's snapshot for transcript syncing: a newer one replaces
// one not yet taken, but a new stream's mark stays until taken.
type latest struct {
	mu    sync.Mutex
	snap  PaseoStatus
	fresh bool
	wake  chan struct{}
}

func (l *latest) put(s PaseoStatus, fresh bool) {
	l.mu.Lock()
	l.snap, l.fresh = s, l.fresh || fresh
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *latest) take() (PaseoStatus, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fresh := l.fresh
	l.fresh = false
	return l.snap, fresh
}
