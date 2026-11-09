package paseo

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Schedules: the daemon runs a prompt on a cadence, each time in a new agent (or in an existing
// one). The daemon pushes nothing when a schedule changes, so a reader lists them again.
//
// A schedule's new-agent config can carry a system prompt, MCP servers (with their credentials)
// and provider options; those are never decoded here.

// Schedule is the part of a schedule Intelligence reads. Runs are only in InspectSchedule's.
type Schedule struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Prompt    string         `json:"prompt"`
	Cadence   Cadence        `json:"cadence"`
	Target    ScheduleTarget `json:"target"`
	Status    string         `json:"status"` // active, paused, completed
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	NextRunAt time.Time      `json:"nextRunAt"`
	LastRunAt time.Time      `json:"lastRunAt"`
	PausedAt  time.Time      `json:"pausedAt"`
	ExpiresAt time.Time      `json:"expiresAt"`
	MaxRuns   int            `json:"maxRuns"` // 0: no limit
	Runs      []ScheduleRun  `json:"runs"`
}

// Cadence is when a schedule runs: every EveryMs, or on a cron Expression in Timezone ("" is the
// daemon's own).
type Cadence struct {
	Type       string `json:"type"` // every, cron
	EveryMs    int64  `json:"everyMs,omitempty"`
	Expression string `json:"expression,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
}

// ScheduleTarget is what a schedule runs in: an existing agent (AgentID), or a new agent each
// time (Config).
type ScheduleTarget struct {
	Type    string         `json:"type"` // agent, new-agent
	AgentID string         `json:"agentId"`
	Config  *ScheduleAgent `json:"config"`
}

// ScheduleAgent is a new-agent schedule's agent.
type ScheduleAgent struct {
	Provider         string `json:"provider"`
	Cwd              string `json:"cwd"`
	Model            string `json:"model,omitempty"`
	ModeID           string `json:"modeId,omitempty"`
	ThinkingOptionID string `json:"thinkingOptionId,omitempty"`
	Isolation        string `json:"isolation,omitempty"`       // local (the directory itself) or worktree (a new git worktree per run)
	ArchiveOnFinish  *bool  `json:"archiveOnFinish,omitempty"` // nil: the daemon's default (archive)
	Title            string `json:"title,omitempty"`
}

// ScheduleRun is one run of a schedule.
type ScheduleRun struct {
	ID           string    `json:"id"`
	ScheduledFor time.Time `json:"scheduledFor"`
	StartedAt    time.Time `json:"startedAt"`
	EndedAt      time.Time `json:"endedAt"`
	Status       string    `json:"status"` // running, succeeded, failed
	AgentID      string    `json:"agentId"`
	WorkspaceID  string    `json:"workspaceId"`
	Output       string    `json:"output"`
	Error        string    `json:"error"`
}

// Schedules lists the daemon's schedules, without their runs.
func (c *Client) Schedules(ctx context.Context) ([]Schedule, error) {
	var p struct {
		Schedules []Schedule `json:"schedules"`
	}
	if err := c.command(ctx, map[string]any{"type": "schedule/list"}, &p); err != nil {
		return nil, err
	}
	return p.Schedules, nil
}

// InspectSchedule reads one schedule with its runs.
func (c *Client) InspectSchedule(ctx context.Context, id string) (Schedule, error) {
	return c.scheduleCommand(ctx, map[string]any{"type": "schedule/inspect", "scheduleId": id})
}

// CreateSchedule is a schedule to create, running in a new agent each time.
type CreateSchedule struct {
	Name        string
	Prompt      string
	Cadence     Cadence
	Agent       ScheduleAgent
	MaxRuns     int       // 0: no limit
	ExpiresAt   time.Time // zero: never
	RunOnCreate bool      // run once now, too (always sent: the daemon's default runs an every-cadence schedule at once)
}

// CreateSchedule creates a schedule and returns it. The daemon takes no idempotency key: a repeat
// makes a second schedule.
func (c *Client) CreateSchedule(ctx context.Context, s CreateSchedule) (Schedule, error) {
	if strings.TrimSpace(s.Prompt) == "" || s.Agent.Provider == "" || s.Agent.Cwd == "" {
		return Schedule{}, errors.New("create schedule: a prompt, a provider and a directory are needed")
	}
	req := map[string]any{"type": "schedule/create", "prompt": s.Prompt, "cadence": s.Cadence,
		"target": map[string]any{"type": "new-agent", "config": s.Agent}}
	if s.Name != "" {
		req["name"] = s.Name
	}
	if s.MaxRuns > 0 {
		req["maxRuns"] = s.MaxRuns
	}
	if !s.ExpiresAt.IsZero() {
		req["expiresAt"] = s.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	req["runOnCreate"] = s.RunOnCreate
	return c.scheduleCommand(ctx, req)
}

// UpdateSchedule changes a schedule: each field that is not nil. A pointer to a zero value
// clears it (no name, no run limit, no expiry).
type UpdateSchedule struct {
	Name      *string
	Prompt    *string
	Cadence   *Cadence
	Agent     *ScheduleAgentPatch // a new-agent schedule's agent
	MaxRuns   *int
	ExpiresAt *time.Time
}

// ScheduleAgentPatch changes a new-agent schedule's agent: each field that is not empty (nil).
type ScheduleAgentPatch struct {
	Provider         string `json:"provider,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	Model            string `json:"model,omitempty"`
	ModeID           string `json:"modeId,omitempty"`
	ThinkingOptionID string `json:"thinkingOptionId,omitempty"`
	Isolation        string `json:"isolation,omitempty"`
	ArchiveOnFinish  *bool  `json:"archiveOnFinish,omitempty"`
}

// UpdateSchedule changes a schedule and returns it.
func (c *Client) UpdateSchedule(ctx context.Context, id string, u UpdateSchedule) (Schedule, error) {
	req := map[string]any{"type": "schedule/update", "scheduleId": id}
	if u.Name != nil {
		req["name"] = nilIfZero(*u.Name)
	}
	if u.Prompt != nil {
		req["prompt"] = *u.Prompt
	}
	if u.Cadence != nil {
		req["cadence"] = *u.Cadence
	}
	if u.Agent != nil {
		req["newAgentConfig"] = *u.Agent
	}
	if u.MaxRuns != nil {
		req["maxRuns"] = nilIfZero(*u.MaxRuns)
	}
	if u.ExpiresAt != nil {
		var v any
		if !u.ExpiresAt.IsZero() {
			v = u.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		req["expiresAt"] = v
	}
	return c.scheduleCommand(ctx, req)
}

// PauseSchedule stops a schedule's runs until it is resumed. Pausing a paused one changes nothing.
func (c *Client) PauseSchedule(ctx context.Context, id string) (Schedule, error) {
	return c.scheduleCommand(ctx, map[string]any{"type": "schedule/pause", "scheduleId": id})
}

// ResumeSchedule starts a paused schedule again. Resuming an active one changes nothing.
func (c *Client) ResumeSchedule(ctx context.Context, id string) (Schedule, error) {
	return c.scheduleCommand(ctx, map[string]any{"type": "schedule/resume", "scheduleId": id})
}

// DeleteSchedule deletes a schedule. One that is not there fails (IsScheduleNotFound).
func (c *Client) DeleteSchedule(ctx context.Context, id string) error {
	return c.command(ctx, map[string]any{"type": "schedule/delete", "scheduleId": id}, nil)
}

// RunScheduleOnce runs a schedule now. The daemon answers only when the run has finished, which
// can take as long as the agent works: to know it started, watch InspectSchedule's runs instead.
func (c *Client) RunScheduleOnce(ctx context.Context, id string) (Schedule, error) {
	return c.scheduleCommand(ctx, map[string]any{"type": "schedule/run-once", "scheduleId": id})
}

// IsScheduleNotFound reports whether err is the daemon saying it has no such schedule.
func IsScheduleNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Schedule not found")
}

func (c *Client) scheduleCommand(ctx context.Context, req map[string]any) (Schedule, error) {
	var p struct {
		Schedule *Schedule `json:"schedule"`
	}
	if err := c.command(ctx, req, &p); err != nil {
		return Schedule{}, err
	}
	if p.Schedule == nil {
		return Schedule{}, errors.New(req["type"].(string) + ": the daemon returned no schedule")
	}
	return *p.Schedule, nil
}

func nilIfZero[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}
