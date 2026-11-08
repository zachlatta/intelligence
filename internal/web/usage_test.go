package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/usage"
)

func seedUsage(t *testing.T, d dbtest.DB) time.Time {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	fixtureFor(t, d).keyFor(t, "host-1") // the host is the test account's
	reg := fleet.New(d.Owner)
	read := func(at time.Time, used float64, reset time.Time) {
		rep := usage.Report{CollectedAt: at, Sources: []usage.Source{
			{Provider: usage.Claude, Accounts: []usage.Account{
				{ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: true, ObservedAt: at, Windows: []usage.Window{
					{ID: "7d", Seconds: usage.Week, UsedPct: used, ResetsAt: reset},
					{ID: "5h", Seconds: usage.FiveHours, UsedPct: 12, ResetsAt: at.Add(2 * time.Hour)}}},
				{ID: "grace@example.com", Label: "grace@example.com", Slot: 2, ObservedAt: at, Error: "cswap usage status: expired"}}},
			{Provider: usage.Codex, Error: "codex meter: 401 Unauthorized"}}}
		if err := reg.RecordUsage(t.Context(), "host-1", rep, at); err != nil {
			t.Fatal(err)
		}
	}
	read(now.Add(-3*24*time.Hour), 95, now.Add(-2*24*time.Hour))
	read(now.Add(-time.Hour), 25, now.Add(84*time.Hour))
	read(now, 30, now.Add(84*time.Hour))
	return now
}

func TestIndexShowsEachLoginsPlan(t *testing.T) {
	h, d := dbServer(t)
	seedUsage(t, d)
	body := get(t, h, "/", nil).Body.String()
	for _, want := range []string{`href="/usage"`, "ada@example.com", "#1", "70% left", "active on host-1", "resets in 3d 12h",
		"grace@example.com", "cswap usage status: expired", "host-1: codex: codex meter: 401 Unauthorized"} {
		if !strings.Contains(body, want) {
			t.Errorf("index misses %q", want)
		}
	}
	if strings.Contains(body, "{claude") {
		t.Errorf("a plan is printed as a Go value")
	}
}

func TestIndexWithoutUsageSaysNothingAboutPlans(t *testing.T) {
	h, _ := dbServer(t)
	if body := get(t, h, "/", nil).Body.String(); strings.Contains(body, `class="plans"`) {
		t.Errorf("an empty plans section is shown")
	}
}

func TestUsagePageChartsHistoryAndWeeklyPeaks(t *testing.T) {
	h, d := dbServer(t)
	seedUsage(t, d)
	rec := get(t, h, "/usage", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<svg", "<polyline", "ada@example.com", "Weekly peaks", "95%", "30%"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage page misses %q", want)
		}
	}
	if rec := get(t, h, "/usage?days=0", nil); rec.Code != 400 {
		t.Errorf("days=0: %d", rec.Code)
	}
}

func TestUsageAPI(t *testing.T) {
	h, d := dbServer(t)
	seedUsage(t, d)
	var v fleet.PlansView
	if err := json.Unmarshal(get(t, h, "/api/usage", nil).Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Plans) != 2 || v.Plans[0].Account != "ada@example.com" || len(v.Plans[0].Windows) != 2 {
		t.Errorf("plans = %+v", v.Plans)
	}
}

func TestChartScalesUsedPercentOverTime(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	svg := string(usageChart([]fleet.Series{{Window: "7d", Points: []fleet.Point{{At: from, Used: 0}, {At: from.Add(24 * time.Hour), Used: 100}}}},
		from, from.Add(48*time.Hour)))
	// x from the left edge to the middle, y from the bottom to the top
	if !strings.Contains(svg, `points="40.0,130.0 340.0,10.0"`) {
		t.Errorf("svg = %s", svg)
	}
}
