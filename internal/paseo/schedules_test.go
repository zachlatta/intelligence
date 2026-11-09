package paseo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestSchedulesListsWithoutRunsOrSecrets(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetSchedules(paseotest.Schedule("sch1", "active"), paseotest.Schedule("sch2", "paused"))
	c := dial(t, f)
	list, err := c.Schedules(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("schedules = %+v", list)
	}
	s := list[0]
	cfg := s.Target.Config
	if s.ID != "sch1" || s.Name != "Schedule sch1" || s.Prompt != "check the thing" || s.Status != "active" ||
		s.Cadence.Type != "cron" || s.Cadence.Expression != "0 7 * * *" || s.Cadence.Timezone != "America/New_York" ||
		s.Target.Type != "new-agent" || cfg == nil || cfg.Provider != "claude" || cfg.Cwd != "/w/app" ||
		cfg.Model != "claude-opus-5-5" || cfg.ModeID != "bypassPermissions" || cfg.ThinkingOptionID != "medium" ||
		cfg.Isolation != "worktree" || cfg.ArchiveOnFinish == nil || !*cfg.ArchiveOnFinish || cfg.Title != "Daily check" ||
		s.NextRunAt.IsZero() || s.LastRunAt.IsZero() || !s.PausedAt.IsZero() || s.MaxRuns != 0 || len(s.Runs) != 0 {
		t.Errorf("schedule = %+v, config = %+v", s, cfg)
	}
	if list[1].Status != "paused" || list[1].PausedAt.IsZero() {
		t.Errorf("paused schedule = %+v", list[1])
	}
}

func TestInspectScheduleHasItsRuns(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetSchedules(paseotest.Schedule("sch1", "active"))
	c := dial(t, f)
	s, err := c.InspectSchedule(ctx5(t), "sch1")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Runs) != 2 {
		t.Fatalf("runs = %+v", s.Runs)
	}
	r := s.Runs[0]
	if r.ID != "run-sch1-1" || r.Status != "succeeded" || r.AgentID != "agent-run-1" || r.WorkspaceID != "wks_run_1" ||
		r.Output != "all good" || r.ScheduledFor.IsZero() || r.StartedAt.IsZero() || r.EndedAt.IsZero() {
		t.Errorf("run = %+v", r)
	}
	if f2 := s.Runs[1]; f2.Status != "failed" || f2.Error != "Working directory /w/gone no longer exists" || f2.AgentID != "" {
		t.Errorf("failed run = %+v", f2)
	}
	if _, err := c.InspectSchedule(ctx5(t), "nope"); err == nil || !strings.Contains(err.Error(), "Schedule not found") {
		t.Errorf("inspect of an unknown schedule: %v", err)
	}
}

func TestCreateScheduleForANewAgent(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	archive := false
	s, err := c.CreateSchedule(ctx5(t), CreateSchedule{Name: "Nightly", Prompt: "tidy up",
		Cadence: Cadence{Type: "cron", Expression: "0 2 * * *", Timezone: "America/New_York"},
		Agent: ScheduleAgent{Provider: "claude", Cwd: "/w/app", Model: "claude-opus-5-5", ModeID: "bypassPermissions",
			ThinkingOptionID: "medium", Isolation: "worktree", ArchiveOnFinish: &archive, Title: "Tidy"},
		MaxRuns: 5, ExpiresAt: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" || s.Name != "Nightly" || s.Status != "active" || s.MaxRuns != 5 {
		t.Errorf("schedule = %+v", s)
	}
	req := f.Requests("schedule/create")[0]
	target := req["target"].(map[string]any)
	cfg := target["config"].(map[string]any)
	cad := req["cadence"].(map[string]any)
	if target["type"] != "new-agent" || cfg["provider"] != "claude" || cfg["cwd"] != "/w/app" || cfg["isolation"] != "worktree" ||
		cfg["archiveOnFinish"] != false || cfg["modeId"] != "bypassPermissions" || cfg["thinkingOptionId"] != "medium" ||
		cfg["title"] != "Tidy" || cad["type"] != "cron" || cad["expression"] != "0 2 * * *" || cad["timezone"] != "America/New_York" ||
		req["maxRuns"] != float64(5) || req["expiresAt"] != "2026-12-01T00:00:00Z" || req["runOnCreate"] != false {
		t.Errorf("request = %v", req)
	}
}

func TestCreateScheduleEvery(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if _, err := c.CreateSchedule(ctx5(t), CreateSchedule{Prompt: "ping", Cadence: Cadence{Type: "every", EveryMs: 1800000},
		Agent: ScheduleAgent{Provider: "claude", Cwd: "/w/app"}, RunOnCreate: true}); err != nil {
		t.Fatal(err)
	}
	req := f.Requests("schedule/create")[0]
	cad := req["cadence"].(map[string]any)
	if cad["type"] != "every" || cad["everyMs"] != float64(1800000) || cad["timezone"] != nil || req["runOnCreate"] != true ||
		req["name"] != nil || req["maxRuns"] != nil {
		t.Errorf("request = %v", req)
	}
	// the daemon's refusal comes back as its reason
	_, err := c.CreateSchedule(ctx5(t), CreateSchedule{Prompt: "ping", Cadence: Cadence{Type: "cron", Expression: "nonsense"},
		Agent: ScheduleAgent{Provider: "claude", Cwd: "/w/app"}})
	if err == nil || !strings.Contains(err.Error(), "Cron expressions must have 5 fields") {
		t.Errorf("bad cron: %v", err)
	}
}

func TestUpdateSchedule(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetSchedules(paseotest.Schedule("sch1", "active"))
	c := dial(t, f)
	name, prompt, archive := "", "new prompt", true
	maxRuns := 0
	expires := time.Time{}
	s, err := c.UpdateSchedule(ctx5(t), "sch1", UpdateSchedule{Name: &name, Prompt: &prompt,
		Cadence: &Cadence{Type: "every", EveryMs: 3600000},
		Agent:   &ScheduleAgentPatch{Model: "claude-sonnet-5-5", Isolation: "local", ArchiveOnFinish: &archive},
		MaxRuns: &maxRuns, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if s.Prompt != "new prompt" || s.Name != "" || s.Cadence.Type != "every" {
		t.Errorf("schedule = %+v", s)
	}
	req := f.Requests("schedule/update")[0]
	cfg := req["newAgentConfig"].(map[string]any)
	if req["scheduleId"] != "sch1" || req["name"] != nil || req["prompt"] != "new prompt" || req["maxRuns"] != nil ||
		req["expiresAt"] != nil || cfg["model"] != "claude-sonnet-5-5" || cfg["isolation"] != "local" || cfg["archiveOnFinish"] != true {
		t.Errorf("request = %v", req)
	}
	for _, k := range []string{"name", "maxRuns", "expiresAt"} { // cleared: sent as null, not left out
		if _, ok := req[k]; !ok {
			t.Errorf("request has no %s: %v", k, req)
		}
	}
	if _, ok := cfg["provider"]; ok {
		t.Errorf("an unchanged provider was sent: %v", cfg)
	}
	// nothing but a prompt: nothing else is sent
	if _, err := c.UpdateSchedule(ctx5(t), "sch1", UpdateSchedule{Prompt: &prompt}); err != nil {
		t.Fatal(err)
	}
	req = f.Requests("schedule/update")[1]
	for _, k := range []string{"name", "cadence", "newAgentConfig", "maxRuns", "expiresAt"} {
		if _, ok := req[k]; ok {
			t.Errorf("an unchanged %s was sent: %v", k, req)
		}
	}
}

func TestPauseResumeDeleteSchedule(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetSchedules(paseotest.Schedule("sch1", "active"))
	c := dial(t, f)
	ctx := ctx5(t)
	if s, err := c.PauseSchedule(ctx, "sch1"); err != nil || s.Status != "paused" {
		t.Fatalf("pause = %+v, %v", s, err)
	}
	if s, err := c.ResumeSchedule(ctx, "sch1"); err != nil || s.Status != "active" {
		t.Fatalf("resume = %+v, %v", s, err)
	}
	if err := c.DeleteSchedule(ctx, "sch1"); err != nil {
		t.Fatal(err)
	}
	err := c.DeleteSchedule(ctx, "sch1")
	if !IsScheduleNotFound(err) {
		t.Errorf("second delete: %v, want not found", err)
	}
	if list, _ := c.Schedules(ctx); len(list) != 0 {
		t.Errorf("schedules after delete = %+v", list)
	}
}

func TestRunScheduleOnceWaitsForTheRun(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetSchedules(paseotest.Schedule("sch1", "active"))
	c := dial(t, f)
	done := make(chan error, 1)
	go func() { _, err := c.RunScheduleOnce(context.Background(), "sch1"); done <- err }()
	// the daemon answers only once the run has finished
	var runID string
	for deadline := time.Now().Add(5 * time.Second); runID == "" && time.Now().Before(deadline); {
		s, err := c.InspectSchedule(ctx5(t), "sch1")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range s.Runs {
			if r.Status == "running" {
				runID = r.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("no running run")
	}
	select {
	case err := <-done:
		t.Fatalf("run-once answered before its run finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.FinishRun("sch1", runID, "succeeded", "done")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
