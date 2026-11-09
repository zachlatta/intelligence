package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// fakeSchedules is a daemon's schedules for a ScheduleSync, and the reports it sent.
type fakeSchedules struct {
	mu       sync.Mutex
	list     []paseo.Schedule
	runs     map[string][]paseo.ScheduleRun
	listErr  error
	sendErr  error
	inspects []string
	sent     []ScheduleReport
}

func (f *fakeSchedules) sync() *ScheduleSync {
	return &ScheduleSync{
		List: func(context.Context) ([]paseo.Schedule, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]paseo.Schedule(nil), f.list...), f.listErr
		},
		Inspect: func(_ context.Context, id string) (paseo.Schedule, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.inspects = append(f.inspects, id)
			for _, s := range f.list {
				if s.ID == id {
					s.Runs = f.runs[id]
					return s, nil
				}
			}
			return paseo.Schedule{}, errors.New("schedule/inspect: Schedule not found: " + id)
		},
		Send: func(_ context.Context, r ScheduleReport) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sendErr != nil {
				return f.sendErr
			}
			f.sent = append(f.sent, r)
			return nil
		},
	}
}

func (f *fakeSchedules) last(t *testing.T, n int) ScheduleReport {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != n {
		t.Fatalf("%d reports sent, want %d", len(f.sent), n)
	}
	return f.sent[n-1]
}

func sched(id string, updated time.Time) paseo.Schedule {
	yes := true
	return paseo.Schedule{ID: id, Name: "S " + id, Prompt: "do it", Status: "active",
		Cadence: paseo.Cadence{Type: "cron", Expression: "0 7 * * *", Timezone: "America/New_York"},
		Target: paseo.ScheduleTarget{Type: "new-agent", Config: &paseo.ScheduleAgent{Provider: "claude", Cwd: "/w/app",
			Model: "claude-opus-5-5", ModeID: "bypassPermissions", ThinkingOptionID: "medium", Isolation: "worktree", ArchiveOnFinish: &yes}},
		UpdatedAt: updated, NextRunAt: updated.Add(time.Hour)}
}

func TestScheduleSyncSendsEverythingFirstThenOnlyWhatChanged(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	f := &fakeSchedules{list: []paseo.Schedule{sched("a", t0), sched("b", t0)},
		runs: map[string][]paseo.ScheduleRun{"a": {{ID: "r1", Status: "succeeded", Output: "ok", StartedAt: t0}}}}
	s := f.sync()
	ctx := context.Background()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.last(t, 1)
	if len(r.Schedules) != 2 || !r.Schedules[0].RunsIncluded || !r.Schedules[1].RunsIncluded || len(r.Schedules[0].Runs) != 1 {
		t.Fatalf("first report = %+v", r)
	}
	a := r.Schedules[0]
	if a.ID != "a" || a.Name != "S a" || a.Cron != "0 7 * * *" || a.Timezone != "America/New_York" || a.Target != "new-agent" ||
		a.Provider != "claude" || a.Cwd != "/w/app" || a.Model != "claude-opus-5-5" || a.Mode != "bypassPermissions" ||
		a.Thinking != "medium" || a.Isolation != "worktree" || !a.ArchiveOnFinish || a.Runs[0].Output != "ok" {
		t.Errorf("schedule = %+v", a)
	}

	// nothing changed: nothing sent
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.last(t, 1)

	// one changed: both listed, only its runs read again
	f.mu.Lock()
	f.list[1] = sched("b", t0.Add(time.Minute))
	f.inspects = nil
	f.mu.Unlock()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r = f.last(t, 2)
	if len(r.Schedules) != 2 || r.Schedules[0].RunsIncluded || !r.Schedules[1].RunsIncluded || fmt.Sprint(f.inspects) != "[b]" {
		t.Errorf("report after a change = %+v, inspected %v", r, f.inspects)
	}

	// one deleted: sent without it
	f.mu.Lock()
	f.list = f.list[:1]
	f.mu.Unlock()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r = f.last(t, 3); len(r.Schedules) != 1 || r.Schedules[0].ID != "a" {
		t.Errorf("report after a delete = %+v", r)
	}

	// a send that fails is sent again
	f.mu.Lock()
	f.list[0] = sched("a", t0.Add(2*time.Minute))
	f.sendErr = ErrNotConnected
	f.mu.Unlock()
	if err := s.Sync(ctx); err == nil {
		t.Fatal("a failed send returned no error")
	}
	f.mu.Lock()
	f.sendErr = nil
	f.mu.Unlock()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r = f.last(t, 4); !r.Schedules[0].RunsIncluded {
		t.Errorf("resent report = %+v", r)
	}

	// a new stream: everything again
	s.Reset()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r = f.last(t, 5); !r.Schedules[0].RunsIncluded {
		t.Errorf("report on a new stream = %+v", r)
	}
}

func TestScheduleSyncReportsADaemonWithoutSchedulesOnce(t *testing.T) {
	f := &fakeSchedules{listErr: errors.New("schedule/list: Unknown request, try upgrading the daemon")}
	s := f.sync()
	ctx := context.Background()
	for range 2 {
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.last(t, 1); !strings.Contains(r.Error, "Unknown request") || r.Schedules != nil {
		t.Errorf("report = %+v", r)
	}
	// not connected to the daemon at all: nothing to say
	f.mu.Lock()
	f.listErr = errNoDaemon
	f.mu.Unlock()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.last(t, 1)
	// it works again
	f.mu.Lock()
	f.listErr = nil
	f.mu.Unlock()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.last(t, 2); r.Error != "" || r.Schedules == nil {
		t.Errorf("report = %+v", r)
	}
}

func TestScheduleSyncSendsTheNewestRunsWithOutputCut(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var runs []paseo.ScheduleRun
	for i := range keepRuns + 10 {
		runs = append(runs, paseo.ScheduleRun{ID: fmt.Sprintf("r%03d", i), Status: "succeeded", StartedAt: t0.Add(time.Duration(i) * time.Hour)})
	}
	runs[len(runs)-1].Output = strings.Repeat("x", maxRunText+100)
	f := &fakeSchedules{list: []paseo.Schedule{sched("a", t0)}, runs: map[string][]paseo.ScheduleRun{"a": runs}}
	if err := f.sync().Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.last(t, 1).Schedules[0].Runs
	if len(got) != keepRuns || got[0].ID != "r010" || got[len(got)-1].ID != fmt.Sprintf("r%03d", keepRuns+9) {
		t.Fatalf("runs: %d, first %s", len(got), got[0].ID)
	}
	if out := got[len(got)-1].Output; len(out) > maxRunText+len(cutMark) || !strings.HasSuffix(out, cutMark) {
		t.Errorf("output not cut: %d bytes", len(out))
	}
}

func TestParseScheduleCommands(t *testing.T) {
	good := map[string]string{
		CreateScheduleKind:              `{"name":"N","prompt":"go","cron":"0 7 * * *","timezone":"America/New_York","agent":{"provider":"claude","cwd":"/w","isolation":"worktree","archiveOnFinish":true},"maxRuns":3,"expiresAt":"2026-12-01T00:00:00Z"}`,
		CreateScheduleKind + " every":   `{"prompt":"go","every":"30m","agent":{"provider":"claude","cwd":"/w"},"runNow":true}`,
		UpdateScheduleKind:              `{"scheduleId":"s1","prompt":"new"}`,
		UpdateScheduleKind + " cadence": `{"scheduleId":"s1","every":"2h"}`,
		UpdateScheduleKind + " clear":   `{"scheduleId":"s1","name":"","maxRuns":0,"expiresAt":""}`,
		UpdateScheduleKind + " agent":   `{"scheduleId":"s1","agent":{"model":"m","isolation":"local","archiveOnFinish":false}}`,
		PauseScheduleKind:               `{"scheduleId":"s1"}`,
		ResumeScheduleKind:              `{"scheduleId":"s1"}`,
		DeleteScheduleKind:              `{"scheduleId":"s1"}`,
		RunScheduleKind:                 `{"scheduleId":"s1"}`,
	}
	for name, args := range good {
		kind, _, _ := strings.Cut(name, " ")
		if _, err := ParseCommand(kind, json.RawMessage(args)); err != nil {
			t.Errorf("%s %s: %v", name, args, err)
		}
	}
	bad := map[string]string{
		CreateScheduleKind + " no prompt":       `{"every":"1h","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " blank prompt":    `{"prompt":"  ","every":"1h","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " no cadence":      `{"prompt":"go","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " two cadences":    `{"prompt":"go","every":"1h","cron":"0 7 * * *","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " too often":       `{"prompt":"go","every":"10s","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " bad every":       `{"prompt":"go","every":"often","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " zone, no cron":   `{"prompt":"go","every":"1h","timezone":"UTC","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " no agent":        `{"prompt":"go","every":"1h"}`,
		CreateScheduleKind + " no provider":     `{"prompt":"go","every":"1h","agent":{"cwd":"/w"}}`,
		CreateScheduleKind + " no cwd":          `{"prompt":"go","every":"1h","agent":{"provider":"claude"}}`,
		CreateScheduleKind + " bad isolation":   `{"prompt":"go","every":"1h","agent":{"provider":"claude","cwd":"/w","isolation":"vm"}}`,
		CreateScheduleKind + " zero runs":       `{"prompt":"go","every":"1h","agent":{"provider":"claude","cwd":"/w"},"maxRuns":0}`,
		CreateScheduleKind + " bad expiry":      `{"prompt":"go","every":"1h","agent":{"provider":"claude","cwd":"/w"},"expiresAt":"soon"}`,
		CreateScheduleKind + " an id":           `{"scheduleId":"s1","prompt":"go","every":"1h","agent":{"provider":"claude","cwd":"/w"}}`,
		CreateScheduleKind + " unknown field":   `{"prompt":"go","every":"1h","agent":{"provider":"claude","cwd":"/w","mcpServers":{}}}`,
		UpdateScheduleKind + " no id":           `{"prompt":"new"}`,
		UpdateScheduleKind + " nothing":         `{"scheduleId":"s1"}`,
		UpdateScheduleKind + " blank prompt":    `{"scheduleId":"s1","prompt":""}`,
		UpdateScheduleKind + " run now":         `{"scheduleId":"s1","prompt":"x","runNow":true}`,
		UpdateScheduleKind + " zone alone":      `{"scheduleId":"s1","timezone":"UTC"}`,
		UpdateScheduleKind + " title":           `{"scheduleId":"s1","agent":{"title":"T"}}`,
		UpdateScheduleKind + " negative runs":   `{"scheduleId":"s1","maxRuns":-1}`,
		PauseScheduleKind + " no id":            `{}`,
		RunScheduleKind + " with a prompt":      `{"scheduleId":"s1","prompt":"x"}`,
		DeleteScheduleKind + " extra":           `{"scheduleId":"s1","agentId":"a1"}`,
		ResumeScheduleKind + " not an object":   `"s1"`,
		CreateScheduleKind + " empty agent obj": `{"prompt":"go","every":"1h","agent":{}}`,
	}
	for name, args := range bad {
		kind, _, _ := strings.Cut(name, " ")
		if _, err := ParseCommand(kind, json.RawMessage(args)); err == nil {
			t.Errorf("%s %s: no error", name, args)
		}
	}
}

func TestExecuteScheduleCommands(t *testing.T) {
	d := daemon(t)
	d.SetSchedules(paseotest.Schedule("sch1", "active"))
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })

	res, err := execute(t, w, CreateScheduleKind, `{"name":"Nightly","prompt":"tidy","every":"1h30m","agent":{"provider":"claude","cwd":"/w/app","model":"claude-opus-5-5","thinking":"medium","mode":"bypassPermissions","isolation":"worktree","archiveOnFinish":false,"title":"Tidy"},"maxRuns":2,"expiresAt":"2026-12-01T00:00:00Z"}`)
	if err != nil || res.ScheduleID == "" {
		t.Fatalf("create = %+v, %v", res, err)
	}
	req := d.Requests("schedule/create")[0]
	cfg := req["target"].(map[string]any)["config"].(map[string]any)
	cad := req["cadence"].(map[string]any)
	if cad["type"] != "every" || cad["everyMs"] != float64(90*60*1000) || cfg["thinkingOptionId"] != "medium" || cfg["modeId"] != "bypassPermissions" ||
		cfg["archiveOnFinish"] != false || cfg["title"] != "Tidy" || req["runOnCreate"] != false || req["maxRuns"] != float64(2) ||
		req["name"] != "Nightly" || req["expiresAt"] != "2026-12-01T00:00:00Z" {
		t.Errorf("create request = %v", req)
	}

	if _, err := execute(t, w, UpdateScheduleKind, `{"scheduleId":"sch1","cron":"0 9 * * 1-5","timezone":"America/Chicago","agent":{"thinking":"high"},"maxRuns":0}`); err != nil {
		t.Fatal(err)
	}
	req = d.Requests("schedule/update")[0]
	if cad := req["cadence"].(map[string]any); cad["type"] != "cron" || cad["expression"] != "0 9 * * 1-5" || cad["timezone"] != "America/Chicago" ||
		req["newAgentConfig"].(map[string]any)["thinkingOptionId"] != "high" || req["maxRuns"] != nil {
		t.Errorf("update request = %v", req)
	}
	if _, ok := req["prompt"]; ok {
		t.Errorf("update sent an unchanged prompt: %v", req)
	}

	if res, err := execute(t, w, PauseScheduleKind, `{"scheduleId":"sch1"}`); err != nil || res.ScheduleID != "sch1" || d.ScheduleStatus("sch1") != "paused" {
		t.Fatalf("pause = %+v, %v, status %s", res, err, d.ScheduleStatus("sch1"))
	}
	if _, err := execute(t, w, ResumeScheduleKind, `{"scheduleId":"sch1"}`); err != nil || d.ScheduleStatus("sch1") != "active" {
		t.Fatalf("resume: %v, status %s", err, d.ScheduleStatus("sch1"))
	}
	// a refusal is the daemon's reason
	if _, err := execute(t, w, PauseScheduleKind, `{"scheduleId":"nope"}`); err == nil || !strings.Contains(err.Error(), "Schedule not found") {
		t.Errorf("pause of an unknown schedule: %v", err)
	}
	// delete is safe to repeat: a schedule already gone is deleted
	for range 2 {
		if _, err := execute(t, w, DeleteScheduleKind, `{"scheduleId":"sch1"}`); err != nil {
			t.Fatal(err)
		}
	}
	if d.ScheduleStatus("sch1") != "" {
		t.Error("schedule not deleted")
	}
}

func TestRunScheduleReturnsOnceTheRunStarts(t *testing.T) {
	d := daemon(t)
	d.SetSchedules(paseotest.Schedule("sch1", "active"))
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	// the daemon answers a run-once only when the run ends, which it never does here
	res, err := execute(t, w, RunScheduleKind, `{"scheduleId":"sch1"}`)
	if err != nil || res.ScheduleID != "sch1" || res.RunID == "" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	// a second run while the first runs: the daemon's refusal
	if _, err := w.Execute(context.Background(), Command{ID: "cmd-2", Kind: RunScheduleKind, Args: json.RawMessage(`{"scheduleId":"sch1"}`)}); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("second run: %v", err)
	}
	d.FinishRun("sch1", res.RunID, "succeeded", "done")
	if _, err := execute(t, w, RunScheduleKind, `{"scheduleId":"nope"}`); err == nil || !strings.Contains(err.Error(), "Schedule not found") {
		t.Errorf("run of an unknown schedule: %v", err)
	}
}

func TestClientSendsSchedulesAndReportsThemBeforeACommandsResult(t *testing.T) {
	s, d := newStreamServer(t), daemon(t)
	d.SetSchedules(paseotest.Schedule("sch1", "active"))
	hostRig(t, s, d)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.frames(SchedulesFrame)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no schedules sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := s.frames(SchedulesFrame)[0].Schedules
	if len(first.Schedules) != 1 || first.Schedules[0].ID != "sch1" || len(first.Schedules[0].Runs) != 2 {
		t.Fatalf("schedules = %+v", first)
	}
	s.push(Frame{Type: CommandFrame, Command: &Command{ID: "cmd-pause", Kind: PauseScheduleKind, Args: json.RawMessage(`{"scheduleId":"sch1"}`)}})
	for {
		var done bool
		paused := false
		for _, f := range s.ordered() {
			switch {
			case f.Type == SchedulesFrame && len(f.Schedules.Schedules) == 1 && f.Schedules.Schedules[0].Status == "paused":
				paused = true
			case f.Type == CommandUpdateFrame && f.Update.ID == "cmd-pause" && f.Update.Status == CommandSucceeded:
				if !paused {
					t.Fatal("the command succeeded before the host sent the paused schedule")
				}
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pause never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ordered is every host frame the server has received, in order.
func (s *streamServer) ordered() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Frame(nil), s.got...)
}
