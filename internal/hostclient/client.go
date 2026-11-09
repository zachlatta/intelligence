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

	StatusEvery time.Duration // re-read the daemon's status (a change is reported); default 15s
	LiveEvery   time.Duration // copy running agents' transcripts; default 2s
	DiffEvery   time.Duration // re-read the diffs of workspaces an agent works in; default 5s
	Settle      time.Duration // gather a burst of daemon updates into one report; default 1s
	Logf        func(format string, args ...any)

	schedules       *ScheduleSync
	scheduleMu      sync.Mutex
	lastScheduleErr string // syncSchedules logs an error once
}

// Run runs the host until ctx ends.
func (b *Client) Run(ctx context.Context) error {
	if b.StatusEvery == 0 {
		b.StatusEvery = 15 * time.Second
	}
	if b.LiveEvery == 0 {
		b.LiveEvery = 2 * time.Second
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
		err := b.Link.Report(sendCtx, Report{Name: b.Name, Version: b.Version, Paseo: snap, CollectedAt: time.Now()})
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
// knows the agents) and, while one is running, every LiveEvery.
func (b *Client) syncTranscripts(ctx context.Context, reported *latest) {
	ts := &TranscriptSync{Host: b.Name, Timeline: b.Watcher.Timeline, Cursors: b.Link.TranscriptCursors, Send: b.Link.Transcript}
	live := time.NewTicker(b.LiveEvery)
	defer live.Stop()
	var snap PaseoStatus
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-reported.wake:
			var fresh bool
			snap, fresh = reported.take()
			if fresh {
				ts.Reset() // the server's copies may have changed while the stream was down
			}
		case <-live.C:
			if !anyRunning(snap.Agents) {
				continue
			}
			snap = b.Watcher.Snapshot()
		}
		if !snap.Connected {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := ts.Sync(syncCtx, snap.Agents)
		cancel()
		if msg := fmt.Sprint(err); err != nil && msg != lastErr && ctx.Err() == nil {
			b.Logf("transcript sync: %v", err)
			lastErr = msg
		} else if err == nil {
			lastErr = ""
		}
	}
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

// runCommands runs the server's commands against the daemon.
func (b *Client) runCommands(ctx context.Context) {
	exec := func(ctx context.Context, cmd Command) (CommandResult, error) {
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
