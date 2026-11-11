package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

var sampleSchedules = hostclient.ScheduleReport{CollectedAt: t0, Schedules: []hostclient.Schedule{
	{ID: "sch-nightly", Name: "Nightly <tidy>", Prompt: "tidy the <repo>", Status: "active", Cron: "0 2 * * *", Timezone: "America/New_York",
		Target: "new-agent", Provider: "claude", Cwd: "/w/app", Model: "claude-opus-5-5", Mode: "bypassPermissions", Thinking: "medium",
		Isolation: "worktree", ArchiveOnFinish: true, CreatedAt: t0.Add(-48 * time.Hour), UpdatedAt: t0, NextRunAt: t0.Add(14 * time.Hour),
		LastRunAt: t0.Add(-time.Hour), RunsIncluded: true, Runs: []hostclient.ScheduleRun{
			{ID: "run-1", Status: "succeeded", StartedAt: t0.Add(-25 * time.Hour), EndedAt: t0.Add(-24 * time.Hour), AgentID: agentA1,
				Output: "All **tidy**: 3 files"},
			{ID: "run-2", Status: "failed", StartedAt: t0.Add(-time.Hour), EndedAt: t0.Add(-time.Hour + time.Minute),
				Error: "Working directory /w/gone no longer exists"},
		}},
	{ID: "sch-ping", Prompt: "ping the service", Status: "paused", EveryMs: 30 * 60 * 1000, Target: "new-agent", Provider: "claude",
		Cwd: "/w/app", PausedAt: t0, RunsIncluded: true},
}}

func sendSchedules(t *testing.T, h http.Handler, rep hostclient.ScheduleReport) {
	t.Helper()
	b := openStream(t, h, "host-1")
	if ack := b.call(hostclient.Frame{Type: hostclient.SchedulesFrame, Schedules: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

func TestStreamStoresSchedules(t *testing.T) {
	h, d := dbServer(t)
	b := openStream(t, h, "host-1") // before the host's first report: schedules do not wait for it
	rep := sampleSchedules
	if ack := b.call(hostclient.Frame{Type: hostclient.SchedulesFrame, Schedules: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var schedules, runs int
	if err := d.Owner.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM schedules), (SELECT count(*) FROM schedule_runs)").
		Scan(&schedules, &runs); err != nil || schedules != 2 || runs != 2 {
		t.Errorf("stored %d schedules, %d runs: %v", schedules, runs, err)
	}
	if ack := b.call(hostclient.Frame{Type: hostclient.SchedulesFrame}); ack.Error == "" {
		t.Errorf("an empty schedules frame was accepted")
	}
}

func TestSchedulesPageListsEverySchedule(t *testing.T) {
	h := reported(t)
	if body := get(t, h, "/schedules", nil).Body.String(); !strings.Contains(body, "No schedules") {
		t.Errorf("empty schedules page: %s", body)
	}
	sendSchedules(t, h, sampleSchedules)
	rec := get(t, h, "/schedules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"host-1", "Nightly &lt;tidy&gt;", `href="/hosts/host-1/schedules/sch-nightly"`,
		"daily at 02:00", "America/New_York", "every 30m", "paused", "failed", "ping the service",
		`href="/hosts/host-1/schedules/new"`, `data-kind="pause_schedule"`, `data-kind="resume_schedule"`, "<thead>",
		`<main class="container page">`, `class="sched-actions"`, `class="table sched-table"`, `data-label="When"`} {
		if !strings.Contains(body, want) {
			t.Errorf("schedules page misses %q", want)
		}
	}
	if strings.Contains(body, "Nightly <tidy>") {
		t.Error("a schedule's name is not escaped")
	}
	if !strings.Contains(get(t, h, "/", nil).Body.String(), `href="/schedules"`) {
		t.Error("the top bar does not link the schedules page")
	}
}

func TestSchedulePageShowsItsRunsAndEditForm(t *testing.T) {
	h := reported(t)
	sendSchedules(t, h, sampleSchedules)
	rec := get(t, h, "/hosts/host-1/schedules/sch-nightly", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Nightly &lt;tidy&gt;", "tidy the &lt;repo&gt;", "0 2 * * *", "worktree", "claude-opus-5-5",
		"All **tidy**: 3 files", "Working directory /w/gone no longer exists", `href="/hosts/host-1/agents/` + agentA1 + `"`,
		`data-kind="run_schedule"`, `data-kind="delete_schedule"`, `id="edit"`, `"update_schedule"`, "2 runs"} {
		if !strings.Contains(body, want) {
			t.Errorf("schedule page misses %q", want)
		}
	}
	for _, path := range []string{"/hosts/host-1/schedules/nope", "/hosts/nohost/schedules/sch-nightly"} {
		if rec := get(t, h, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}
}

func TestNewSchedulePage(t *testing.T) {
	h := reported(t)
	rec := get(t, h, "/hosts/host-1/schedules/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-kind="create_schedule"`, `name="prompt"`, `name="cron"`, `name="every"`, `name="timezone"`,
		`name="isolation"`, `name="archiveOnFinish"`, `name="maxRuns"`, `name="runNow"`, "claude-opus-5-5", "/w/docs"} {
		if !strings.Contains(body, want) {
			t.Errorf("new schedule page misses %q", want)
		}
	}
	if rec := get(t, h, "/hosts/nohost/schedules/new", nil); rec.Code != http.StatusNotFound {
		t.Errorf("new schedule on an unknown host: %d", rec.Code)
	}
}

func TestScheduleCommandsGoThroughTheCommandsAPI(t *testing.T) {
	h := reported(t)
	for body, want := range map[string]int{
		`{"kind":"create_schedule","args":{"prompt":"go","cron":"0 7 * * *","agent":{"provider":"claude","cwd":"/w/app"}}}`: http.StatusAccepted,
		`{"kind":"pause_schedule","args":{"scheduleId":"sch-nightly"}}`:                                                     http.StatusAccepted,
		`{"kind":"run_schedule","args":{"scheduleId":"sch-nightly"}}`:                                                       http.StatusAccepted,
		`{"kind":"update_schedule","args":{"scheduleId":"sch-nightly"}}`:                                                    http.StatusBadRequest,
		`{"kind":"create_schedule","args":{"prompt":"go","every":"5s","agent":{"provider":"claude","cwd":"/w/app"}}}`:       http.StatusBadRequest,
	} {
		if rec := call(t, h, http.MethodPost, "/api/hosts/host-1/commands", body, sameOriginFetch); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", body, rec.Code, rec.Body, want)
		}
	}
}

func TestAnotherAccountSeesNoSchedules(t *testing.T) {
	h := reported(t)
	sendSchedules(t, h, sampleSchedules)
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	if body := call(t, h, http.MethodGet, "/schedules", "", nil, asBob).Body.String(); strings.Contains(body, "Nightly") || strings.Contains(body, "host-1") {
		t.Error("bob's schedules page shows someone's schedules")
	}
	for _, path := range []string{"/hosts/host-1/schedules/sch-nightly", "/hosts/host-1/schedules/new"} {
		if rec := call(t, h, http.MethodGet, path, "", nil, asBob); rec.Code != http.StatusNotFound {
			t.Errorf("bob GET %s: %d, want 404", path, rec.Code)
		}
	}
	rec := call(t, h, http.MethodGet, "/api/sql?q=SELECT+count(*)+FROM+schedule_runs", "", nil, asBob)
	if !strings.Contains(rec.Body.String(), `"rows":[[0]]`) {
		t.Errorf("bob reads someone's runs: %s", rec.Body)
	}
}

func TestDescribeCadence(t *testing.T) {
	for _, c := range []struct {
		s    hostclient.Schedule
		want string
	}{
		{hostclient.Schedule{EveryMs: 30 * 60 * 1000}, "every 30m"},
		{hostclient.Schedule{EveryMs: 2 * 3600 * 1000}, "every 2h"},
		{hostclient.Schedule{EveryMs: 90 * 60 * 1000}, "every 1h30m"},
		{hostclient.Schedule{Cron: "0 7 * * *"}, "daily at 07:00"},
		{hostclient.Schedule{Cron: "30 17 * * 1-5"}, "weekdays at 17:30"},
		{hostclient.Schedule{Cron: "0 3 * * 1"}, "Mondays at 03:00"},
		{hostclient.Schedule{Cron: "0 */3 * * *"}, "cron 0 */3 * * *"},
		{hostclient.Schedule{Cron: "15 * * * *"}, "hourly at :15"},
	} {
		if got := cadence(c.s); got != c.want {
			t.Errorf("cadence(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}
