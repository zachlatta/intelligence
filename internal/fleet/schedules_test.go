package fleet

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func schedReport(runs ...hostclient.ScheduleRun) hostclient.ScheduleReport {
	return hostclient.ScheduleReport{CollectedAt: t1, Schedules: []hostclient.Schedule{
		{ID: "s1", Name: "Nightly", Prompt: "tidy up", Status: "active", Cron: "0 2 * * *", Timezone: "America/New_York",
			Target: "new-agent", Provider: "claude", Cwd: "/w/app", Model: "claude-opus-5-5", Mode: "bypassPermissions",
			Thinking: "medium", Isolation: "worktree", ArchiveOnFinish: true, Title: "Tidy", MaxRuns: 10,
			CreatedAt: t1.Add(-48 * time.Hour), UpdatedAt: t1, NextRunAt: t1.Add(14 * time.Hour), LastRunAt: t1.Add(-time.Hour),
			RunsIncluded: true, Runs: runs},
		{ID: "s2", Prompt: "ping", Status: "paused", EveryMs: 1800000, Target: "new-agent", Provider: "codex", Cwd: "/w/x",
			PausedAt: t1, RunsIncluded: true},
	}}
}

func schedRun(id, status string, started time.Time) hostclient.ScheduleRun {
	return hostclient.ScheduleRun{ID: id, Status: status, StartedAt: started, ScheduledFor: started, EndedAt: started.Add(5 * time.Minute),
		AgentID: "agent-" + id, WorkspaceID: "wks-" + id, Output: "done " + id}
}

func TestRecordSchedulesStoresSchedulesAndKeepsTheirRuns(t *testing.T) {
	r := New(dbtestOwner(t))
	if err := r.Record(ctx, full(), t1); err != nil {
		t.Fatal(err)
	}
	rep := schedReport(schedRun("r1", "succeeded", t1.Add(-26*time.Hour)), schedRun("r2", "failed", t1.Add(-2*time.Hour)))
	if err := r.RecordSchedules(ctx, "host-1", rep, t1); err != nil {
		t.Fatal(err)
	}
	list, err := r.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("schedules = %+v", list)
	}
	s := list[0]
	if s.Host != "host-1" || s.ID != "s1" || s.Name != "Nightly" || s.Cron != "0 2 * * *" || s.Timezone != "America/New_York" ||
		s.Provider != "claude" || s.Isolation != "worktree" || !s.ArchiveOnFinish || s.MaxRuns != 10 || s.Title != "Tidy" ||
		!s.NextRunAt.Equal(t1.Add(14*time.Hour)) || s.RunCount != 2 || s.FailedRuns != 1 || s.LastRun == nil ||
		s.LastRun.ID != "r2" || s.LastRun.Status != "failed" {
		t.Errorf("schedule = %+v, last run %+v", s, s.LastRun)
	}
	if list[1].ID != "s2" || list[1].EveryMs != 1800000 || list[1].Status != "paused" || list[1].MaxRuns != 0 || list[1].LastRun != nil {
		t.Errorf("schedule 2 = %+v", list[1])
	}

	// later: s1 changed and sends only its newest runs; s2 unchanged, without runs; the history stays
	rep = schedReport(schedRun("r2", "failed", t1.Add(-2*time.Hour)), schedRun("r3", "running", t1.Add(time.Hour)))
	rep.Schedules[0].Prompt = "tidy up more"
	rep.Schedules[1].RunsIncluded = false
	if err := r.RecordSchedules(ctx, "host-1", rep, t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	one, runs, err := r.Schedule(ctx, "host-1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if one.Prompt != "tidy up more" || len(runs) != 3 || runs[0].ID != "r3" || runs[2].ID != "r1" || !one.Running ||
		runs[1].Output != "done r2" || runs[1].AgentID != "agent-r2" {
		t.Errorf("schedule = %+v, runs = %+v", one, runs)
	}

	// a failed list keeps what the server has, and says why
	if err := r.RecordSchedules(ctx, "host-1", hostclient.ScheduleReport{CollectedAt: t1, Error: "Unknown request"}, t1.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var schedErr string
	r.p.QueryRow(ctx, "SELECT schedules_error FROM hosts WHERE name = 'host-1'").Scan(&schedErr)
	if list, _ := r.Schedules(ctx); len(list) != 2 || schedErr != "Unknown request" {
		t.Errorf("after a failed list: %d schedules, error %q", len(list), schedErr)
	}

	// a schedule gone from the host goes, with its runs
	rep = schedReport()
	rep.Schedules = rep.Schedules[1:]
	if err := r.RecordSchedules(ctx, "host-1", rep, t1.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var schedules, runCount int
	r.p.QueryRow(ctx, "SELECT (SELECT count(*) FROM schedules), (SELECT count(*) FROM schedule_runs), (SELECT schedules_error FROM hosts)").
		Scan(&schedules, &runCount, &schedErr)
	if schedules != 1 || runCount != 0 || schedErr != "" {
		t.Errorf("after a delete: %d schedules, %d runs, error %q", schedules, runCount, schedErr)
	}
	if _, _, err := r.Schedule(ctx, "host-1", "s1"); err != ErrNoSchedule {
		t.Errorf("a deleted schedule: %v", err)
	}
}

func TestRecordSchedulesForABoxThatHasNotReported(t *testing.T) {
	r := New(dbtestOwner(t))
	// a stream attaches before the first report; schedules can come first
	if err := r.Attach(ctx, "host-2", "00000000-0000-0000-0000-000000000001", t1); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordSchedules(ctx, "host-2", schedReport(), t1); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.Schedules(ctx); len(list) != 2 {
		t.Errorf("schedules = %+v", list)
	}
}
