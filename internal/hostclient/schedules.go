package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// Schedules. The daemon runs a prompt on a cadence, each time in a new agent; it pushes nothing
// when a schedule changes, so the host lists them on every status read (and after each schedule
// command) and sends them up the stream when they changed. A schedule's runs are read again only
// when the schedule changed, and only its newest keepRuns go up: the server keeps the history.

// ScheduleReport is the daemon's schedules at one moment. With Error, the daemon could not list
// them (one too old to have schedules) and Schedules is nil: the server keeps what it had.
type ScheduleReport struct {
	CollectedAt time.Time  `json:"collectedAt"`
	Error       string     `json:"error,omitempty"`
	Schedules   []Schedule `json:"schedules"`
}

// Schedule is one of the daemon's schedules. Runs are only sent (RunsIncluded) when the schedule
// changed since the server last stored it.
type Schedule struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Status string `json:"status"` // active, paused, completed
	// cadence: every EveryMs, or on Cron in Timezone ("" the daemon's own)
	EveryMs  int64  `json:"everyMs,omitempty"`
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	// target: a new agent each run (new-agent, with the fields below) or an existing agent (AgentID)
	Target          string `json:"target"`
	AgentID         string `json:"agentId,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	Model           string `json:"model,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Thinking        string `json:"thinking,omitempty"`
	Isolation       string `json:"isolation,omitempty"` // local or worktree
	ArchiveOnFinish bool   `json:"archiveOnFinish"`
	Title           string `json:"title,omitempty"`

	MaxRuns      int           `json:"maxRuns,omitempty"`
	CreatedAt    time.Time     `json:"createdAt,omitzero"`
	UpdatedAt    time.Time     `json:"updatedAt,omitzero"`
	NextRunAt    time.Time     `json:"nextRunAt,omitzero"`
	LastRunAt    time.Time     `json:"lastRunAt,omitzero"`
	PausedAt     time.Time     `json:"pausedAt,omitzero"`
	ExpiresAt    time.Time     `json:"expiresAt,omitzero"`
	RunsIncluded bool          `json:"runsIncluded"`
	Runs         []ScheduleRun `json:"runs,omitempty"`
}

// ScheduleRun is one run of a schedule: the agent it ran in, and how it ended.
type ScheduleRun struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"` // running, succeeded, failed
	ScheduledFor time.Time `json:"scheduledFor,omitzero"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	EndedAt      time.Time `json:"endedAt,omitzero"`
	AgentID      string    `json:"agentId,omitempty"`
	WorkspaceID  string    `json:"workspaceId,omitempty"`
	Output       string    `json:"output,omitempty"` // the agent's last message, cut at maxRunText
	Error        string    `json:"error,omitempty"`
}

const (
	keepRuns   = 50       // the newest runs sent per changed schedule
	maxRunText = 64 << 10 // a run's output or error is cut after this many bytes
	cutMark    = "\n[cut]"
)

// errNoDaemon: the host has no connection to its daemon, so it has nothing to say about schedules.
var errNoDaemon = errors.New("not connected to the Paseo daemon")

// ScheduleSync sends the daemon's schedules to the server when they change.
type ScheduleSync struct {
	List    func(ctx context.Context) ([]paseo.Schedule, error)
	Inspect func(ctx context.Context, id string) (paseo.Schedule, error)
	Send    func(ctx context.Context, r ScheduleReport) error
	Now     func() time.Time

	mu      sync.Mutex
	stored  map[string]string // what the server has: each schedule's summary, as JSON; nil before the first send
	lastErr string            // the list error the server has
}

// Reset forgets what the server has (a new stream), so the next Sync sends everything.
func (s *ScheduleSync) Reset() {
	s.mu.Lock()
	s.stored, s.lastErr = nil, ""
	s.mu.Unlock()
}

// Sync lists the schedules and sends them if they changed since the server last stored them.
func (s *ScheduleSync) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	list, err := s.List(ctx)
	if errors.Is(err, errNoDaemon) {
		return nil
	}
	if err != nil {
		msg := err.Error()
		if msg == s.lastErr && s.stored != nil {
			return nil
		}
		if err := s.Send(ctx, ScheduleReport{CollectedAt: now(), Error: msg}); err != nil {
			return err
		}
		s.stored, s.lastErr = map[string]string{}, msg
		return nil
	}
	rep := ScheduleReport{CollectedAt: now(), Schedules: []Schedule{}}
	next := make(map[string]string, len(list))
	changed := s.stored == nil || s.lastErr != "" || len(list) != len(s.stored)
	for _, ps := range list {
		b, _ := json.Marshal(ps)
		key := string(b)
		sc := scheduleOf(ps)
		if s.stored[ps.ID] != key {
			full, err := s.Inspect(ctx, ps.ID)
			if paseo.IsScheduleNotFound(err) {
				changed = true
				continue // deleted since the list
			}
			if err != nil {
				return err
			}
			sc = scheduleOf(full) // newer than the list's
			sc.RunsIncluded, sc.Runs = true, runsOf(full.Runs)
			fb, _ := json.Marshal(scheduleSummary(full))
			key, changed = string(fb), true
		}
		next[ps.ID] = key
		rep.Schedules = append(rep.Schedules, sc)
	}
	if !changed {
		return nil
	}
	if err := s.Send(ctx, rep); err != nil {
		return err
	}
	s.stored, s.lastErr = next, ""
	return nil
}

// scheduleSummary is s as the list has it, without runs.
func scheduleSummary(s paseo.Schedule) paseo.Schedule { s.Runs = nil; return s }

func scheduleOf(p paseo.Schedule) Schedule {
	s := Schedule{ID: p.ID, Name: p.Name, Prompt: p.Prompt, Status: p.Status, Target: p.Target.Type, AgentID: p.Target.AgentID,
		MaxRuns: p.MaxRuns, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, NextRunAt: p.NextRunAt, LastRunAt: p.LastRunAt,
		PausedAt: p.PausedAt, ExpiresAt: p.ExpiresAt}
	switch p.Cadence.Type {
	case "every":
		s.EveryMs = p.Cadence.EveryMs
	case "cron":
		s.Cron, s.Timezone = p.Cadence.Expression, p.Cadence.Timezone
	}
	if c := p.Target.Config; c != nil {
		s.Provider, s.Cwd, s.Model, s.Mode, s.Thinking, s.Isolation, s.Title = c.Provider, c.Cwd, c.Model, c.ModeID,
			c.ThinkingOptionID, c.Isolation, c.Title
		s.ArchiveOnFinish = c.ArchiveOnFinish == nil || *c.ArchiveOnFinish // the daemon archives by default
	}
	return s
}

// runsOf is the newest keepRuns runs, oldest first, with long text cut.
func runsOf(runs []paseo.ScheduleRun) []ScheduleRun {
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b paseo.ScheduleRun) int { return a.StartedAt.Compare(b.StartedAt) })
	if len(runs) > keepRuns {
		runs = runs[len(runs)-keepRuns:]
	}
	out := make([]ScheduleRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, ScheduleRun{ID: r.ID, Status: r.Status, ScheduledFor: r.ScheduledFor, StartedAt: r.StartedAt,
			EndedAt: r.EndedAt, AgentID: r.AgentID, WorkspaceID: r.WorkspaceID, Output: cut(r.Output), Error: cut(r.Error)})
	}
	return out
}

func cut(s string) string { return cutAt(s, maxRunText) }

// cutAt is s cut after n bytes (at a character boundary), marked as cut.
func cutAt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + cutMark
}

// Schedules lists the daemon's schedules on the current connection.
func (w *Watcher) Schedules(ctx context.Context) ([]paseo.Schedule, error) {
	c := w.current()
	if c == nil {
		return nil, errNoDaemon
	}
	return c.Schedules(ctx)
}

// InspectSchedule reads one schedule, with its runs, on the current connection.
func (w *Watcher) InspectSchedule(ctx context.Context, id string) (paseo.Schedule, error) {
	c := w.current()
	if c == nil {
		return paseo.Schedule{}, errNoDaemon
	}
	return c.InspectSchedule(ctx, id)
}

func (w *Watcher) current() *paseo.Client {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.client
}

// The schedule command kinds.
const (
	CreateScheduleKind = "create_schedule"
	UpdateScheduleKind = "update_schedule"
	PauseScheduleKind  = "pause_schedule"
	ResumeScheduleKind = "resume_schedule"
	DeleteScheduleKind = "delete_schedule"
	RunScheduleKind    = "run_schedule"
)

// IsScheduleKind reports whether kind changes a schedule.
func IsScheduleKind(kind string) bool {
	switch kind {
	case CreateScheduleKind, UpdateScheduleKind, PauseScheduleKind, ResumeScheduleKind, DeleteScheduleKind, RunScheduleKind:
		return true
	}
	return false
}

// MinScheduleEvery is the shortest every-cadence a schedule can have.
const MinScheduleEvery = time.Minute

// ScheduleArgs: create_schedule (a prompt, a cadence and an agent; runNow runs it once now too)
// and update_schedule (scheduleId, and whatever changes). A cadence is every (a duration, such
// as "30m") or cron with an optional timezone. For update, a cadence replaces the old one whole;
// name "" clears the name, maxRuns 0 the run limit and expiresAt "" the expiry.
type ScheduleArgs struct {
	ScheduleID string             `json:"scheduleId,omitempty"`
	Name       *string            `json:"name,omitempty"`
	Prompt     *string            `json:"prompt,omitempty"`
	Every      string             `json:"every,omitempty"`
	Cron       string             `json:"cron,omitempty"`
	Timezone   string             `json:"timezone,omitempty"`
	Agent      *ScheduleAgentArgs `json:"agent,omitempty"`
	MaxRuns    *int               `json:"maxRuns,omitempty"`
	ExpiresAt  *string            `json:"expiresAt,omitempty"` // RFC 3339
	RunNow     bool               `json:"runNow,omitempty"`
}

// ScheduleAgentArgs is the agent a schedule starts each run. For update, "" (nil) is unchanged,
// and the title cannot change.
type ScheduleAgentArgs struct {
	Provider        string `json:"provider,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	Model           string `json:"model,omitempty"`
	Thinking        string `json:"thinking,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Isolation       string `json:"isolation,omitempty"` // local (the directory itself) or worktree (a new git worktree per run)
	ArchiveOnFinish *bool  `json:"archiveOnFinish,omitempty"`
	Title           string `json:"title,omitempty"`
}

// ScheduleIDArgs name a schedule: pause_schedule, resume_schedule, delete_schedule, run_schedule.
type ScheduleIDArgs struct {
	ScheduleID string `json:"scheduleId"`
}

// parseSchedule checks a schedule command's args, decoded strictly by decode.
func parseSchedule(kind string, decode func(any) error) (any, error) {
	if kind != CreateScheduleKind && kind != UpdateScheduleKind {
		var a ScheduleIDArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		if a.ScheduleID == "" {
			return nil, fmt.Errorf("%s needs scheduleId", kind)
		}
		return a, nil
	}
	var a ScheduleArgs
	if err := decode(&a); err != nil {
		return nil, err
	}
	if _, err := a.cadence(); err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	if a.Prompt != nil && strings.TrimSpace(*a.Prompt) == "" {
		return nil, fmt.Errorf("%s: the prompt is empty", kind)
	}
	if a.ExpiresAt != nil && *a.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("%s: expiresAt is not an RFC 3339 time", kind)
		}
	}
	if ag := a.Agent; ag != nil && ag.Isolation != "" && ag.Isolation != "local" && ag.Isolation != "worktree" {
		return nil, fmt.Errorf("%s: isolation %q is not local or worktree", kind, ag.Isolation)
	}
	if kind == CreateScheduleKind {
		switch {
		case a.ScheduleID != "":
			return nil, errors.New("create_schedule takes no scheduleId")
		case a.Prompt == nil:
			return nil, errors.New("create_schedule needs a prompt")
		case a.Every == "" && a.Cron == "":
			return nil, errors.New("create_schedule needs every or cron")
		case a.Agent == nil || a.Agent.Provider == "" || a.Agent.Cwd == "":
			return nil, errors.New("create_schedule needs an agent with a provider and a cwd")
		case a.MaxRuns != nil && *a.MaxRuns < 1:
			return nil, errors.New("create_schedule: maxRuns is at least 1")
		case a.ExpiresAt != nil && *a.ExpiresAt == "":
			return nil, errors.New("create_schedule: expiresAt is empty")
		}
		return a, nil
	}
	switch {
	case a.ScheduleID == "":
		return nil, errors.New("update_schedule needs scheduleId")
	case a.RunNow:
		return nil, errors.New("update_schedule takes no runNow (run_schedule runs it)")
	case a.MaxRuns != nil && *a.MaxRuns < 0:
		return nil, errors.New("update_schedule: maxRuns is 0 (no limit) or more")
	case a.Agent != nil && a.Agent.Title != "":
		return nil, errors.New("update_schedule cannot change the agent's title")
	case a.Agent != nil && *a.Agent == (ScheduleAgentArgs{}):
		return nil, errors.New("update_schedule: agent changes nothing")
	case a.Name == nil && a.Prompt == nil && a.Every == "" && a.Cron == "" && a.Agent == nil && a.MaxRuns == nil && a.ExpiresAt == nil:
		return nil, errors.New("update_schedule changes nothing")
	}
	return a, nil
}

// cadence is the args' cadence, nil for none.
func (a ScheduleArgs) cadence() (*paseo.Cadence, error) {
	switch {
	case a.Every != "" && a.Cron != "":
		return nil, errors.New("every or cron, not both")
	case a.Timezone != "" && a.Cron == "":
		return nil, errors.New("a timezone is for a cron cadence")
	case a.Every != "":
		d, err := time.ParseDuration(a.Every)
		if err != nil {
			return nil, fmt.Errorf("every %q is not a duration such as 30m or 2h", a.Every)
		}
		if d < MinScheduleEvery {
			return nil, fmt.Errorf("every is at least %s", MinScheduleEvery)
		}
		return &paseo.Cadence{Type: "every", EveryMs: d.Milliseconds()}, nil
	case a.Cron != "":
		return &paseo.Cadence{Type: "cron", Expression: strings.TrimSpace(a.Cron), Timezone: a.Timezone}, nil
	}
	return nil, nil
}

func (a ScheduleArgs) expiry() *time.Time {
	if a.ExpiresAt == nil {
		return nil
	}
	var t time.Time
	if *a.ExpiresAt != "" {
		t, _ = time.Parse(time.RFC3339, *a.ExpiresAt)
	}
	return &t
}

// executeSchedule runs a schedule command on c.
func executeSchedule(ctx context.Context, c *paseo.Client, cmd Command, parsed any) (CommandResult, error) {
	switch a := parsed.(type) {
	case ScheduleArgs:
		cad, _ := a.cadence()
		if cmd.Kind == CreateScheduleKind {
			ag := a.Agent
			cs := paseo.CreateSchedule{Prompt: *a.Prompt, Cadence: *cad, RunOnCreate: a.RunNow,
				Agent: paseo.ScheduleAgent{Provider: ag.Provider, Cwd: ag.Cwd, Model: ag.Model, ModeID: ag.Mode, ThinkingOptionID: ag.Thinking,
					Isolation: ag.Isolation, ArchiveOnFinish: ag.ArchiveOnFinish, Title: ag.Title}}
			if a.Name != nil {
				cs.Name = *a.Name
			}
			if a.MaxRuns != nil {
				cs.MaxRuns = *a.MaxRuns
			}
			if t := a.expiry(); t != nil {
				cs.ExpiresAt = *t
			}
			s, err := c.CreateSchedule(ctx, cs)
			return CommandResult{ScheduleID: s.ID}, err
		}
		u := paseo.UpdateSchedule{Name: a.Name, Prompt: a.Prompt, Cadence: cad, MaxRuns: a.MaxRuns, ExpiresAt: a.expiry()}
		if ag := a.Agent; ag != nil {
			u.Agent = &paseo.ScheduleAgentPatch{Provider: ag.Provider, Cwd: ag.Cwd, Model: ag.Model, ModeID: ag.Mode,
				ThinkingOptionID: ag.Thinking, Isolation: ag.Isolation, ArchiveOnFinish: ag.ArchiveOnFinish}
		}
		_, err := c.UpdateSchedule(ctx, a.ScheduleID, u)
		return CommandResult{ScheduleID: a.ScheduleID}, err
	case ScheduleIDArgs:
		r := CommandResult{ScheduleID: a.ScheduleID}
		var err error
		switch cmd.Kind {
		case PauseScheduleKind:
			_, err = c.PauseSchedule(ctx, a.ScheduleID)
		case ResumeScheduleKind:
			_, err = c.ResumeSchedule(ctx, a.ScheduleID)
		case DeleteScheduleKind:
			if err = c.DeleteSchedule(ctx, a.ScheduleID); paseo.IsScheduleNotFound(err) {
				err = nil // already gone: a repeat of a delete that ran
			}
		case RunScheduleKind:
			r.RunID, err = runScheduleOnce(ctx, c, a.ScheduleID)
		}
		return r, err
	}
	return CommandResult{}, fmt.Errorf("unknown command kind %q", cmd.Kind)
}

// runScheduleOnce starts a run of the schedule and returns its id once the daemon shows it
// running. The daemon answers a run-once only when the run ends, which can take as long as the
// agent works, so the answer is not waited for (unless it is a refusal, which comes at once).
func runScheduleOnce(ctx context.Context, c *paseo.Client, id string) (string, error) {
	before, err := c.InspectSchedule(ctx, id)
	if err != nil {
		return "", err
	}
	known := map[string]bool{}
	for _, r := range before.Runs {
		known[r.ID] = true
	}
	answered := make(chan error, 1)
	go func() {
		// lives until the run ends or the connection does; nobody waits for it after a run shows up
		rctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		_, err := c.RunScheduleOnce(rctx, id)
		answered <- err
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("run of schedule %s did not start: %w", id, ctx.Err())
		case err := <-answered:
			if err != nil {
				return "", err
			}
			answered = nil // a run that finished at once: its id is in the runs
		case <-tick.C:
		}
		s, err := c.InspectSchedule(ctx, id)
		if err != nil {
			return "", err
		}
		for i := len(s.Runs) - 1; i >= 0; i-- {
			if !known[s.Runs[i].ID] {
				return s.Runs[i].ID, nil
			}
		}
	}
}
