package web

import (
	"encoding/json"
	"strconv"
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
	t.Parallel()
	h, d := dbServer(t)
	seedUsage(t, d)
	body := get(t, h, "/hosts", nil).Body.String()
	for _, want := range []string{`href="/usage"`, "ada@example.com", "#1", "70% left", "host-1 #1 · active", "resets in 3d 12h",
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
	t.Parallel()
	h, _ := dbServer(t)
	if body := get(t, h, "/hosts", nil).Body.String(); strings.Contains(body, `class="plans"`) {
		t.Errorf("an empty plans section is shown")
	}
}

func TestUsagePageChartsHistoryAndWeeklyPeaks(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	seedUsage(t, d)
	rec := get(t, h, "/usage", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<svg", "<polyline", "ada@example.com", "Weekly peaks", "95%", "30%", "<thead>"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage page misses %q", want)
		}
	}
	if rec := get(t, h, "/usage?days=0", nil); rec.Code != 400 {
		t.Errorf("days=0: %d", rec.Code)
	}
}

func TestUsagePageFoldsOlderWeeklyPeaks(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	fixtureFor(t, d).keyFor(t, "host-1")
	reg := fleet.New(d.Owner)
	for w := 6; w >= 1; w-- {
		at := now.Add(-time.Duration(w*7) * 24 * time.Hour)
		rep := usage.Report{CollectedAt: at, Sources: []usage.Source{{Provider: usage.Claude, Accounts: []usage.Account{{
			ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: true, ObservedAt: at,
			Windows: []usage.Window{{ID: "7d", Seconds: usage.Week, UsedPct: float64(30 + w), ResetsAt: at.Add(24 * time.Hour)}},
		}}}}}
		if err := reg.RecordUsage(t.Context(), "host-1", rep, at); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, h, "/usage", nil).Body.String()
	for _, want := range []string{`class="more-peaks"`, "Show 2 older weeks", "31%", "36%"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage page misses %q", want)
		}
	}
}

func TestUsageAPI(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	seedUsage(t, d)
	var v fleet.PlansView
	if err := json.Unmarshal(get(t, h, "/api/usage", nil).Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Plans) != 2 || v.Plans[0].Account != "ada@example.com" || len(v.Plans[0].Windows) != 2 {
		t.Errorf("plans = %+v", v.Plans)
	}
	if len(v.Plans[0].Hosts) != 1 || v.Plans[0].Hosts[0].Host != "host-1" {
		t.Errorf("plan hosts = %+v", v.Plans[0].Hosts)
	}
}

func TestIndexAndUsageShowPerHostPlanAuth(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := fixtureFor(t, d)
	f.keyFor(t, "host-1")
	f.keyFor(t, "host-2")
	reg := fleet.New(d.Owner)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO hosts (name, connected, last_seen, stream_id)
		VALUES ('host-1', true, $1, gen_random_uuid()), ('host-2', true, $1, gen_random_uuid())
		ON CONFLICT (name) DO UPDATE SET connected = true, last_seen = $1, stream_id = gen_random_uuid()`, now); err != nil {
		t.Fatal(err)
	}
	rep1 := usage.Report{CollectedAt: now, Sources: []usage.Source{{Provider: usage.Claude, Accounts: []usage.Account{
		{ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: true, AuthSource: usage.AuthCswap, ObservedAt: now,
			Windows: []usage.Window{{ID: "7d", Seconds: usage.Week, UsedPct: 30, ResetsAt: now.Add(84 * time.Hour)}}},
	}}}}
	rep2 := usage.Report{CollectedAt: now, Sources: []usage.Source{{Provider: usage.Claude, Accounts: []usage.Account{
		{ID: "ada@example.com", Label: "ada@example.com", Active: true, AuthSource: usage.AuthClaudeCLI, ObservedAt: now},
	}}}}
	if err := reg.RecordUsage(t.Context(), "host-1", rep1, now); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordUsage(t.Context(), "host-2", rep2, now); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hosts", "/usage"} {
		body := get(t, h, path, nil).Body.String()
		for _, want := range []string{"plan-hosts", "host-1 #1", "host-2", "active"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s misses %q", path, want)
			}
		}
		// The per-host badges carry each host's slot and state, so the header repeats neither:
		// one host's slot or "active on" a host whose badge says otherwise would mislead.
		for _, unwanted := range []string{`<span class="badge">claude #1</span>`, "active on host-1"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s header repeats per-host state %q", path, unwanted)
			}
		}
	}
}

func TestChartScalesUsedPercentOverTime(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	svg := string(usageChart([]fleet.Series{{Window: "7d", Points: []fleet.Point{{At: from, Used: 0}, {At: from.Add(24 * time.Hour), Used: 100}}}},
		from, from.Add(48*time.Hour)))
	// x from the left edge to the middle, y from the bottom to the top
	if !strings.Contains(svg, `points="40.0,130.0 340.0,10.0"`) {
		t.Errorf("svg = %s", svg)
	}
}

// TestChartColorsAreNotMostlyBlue: the first series (and so host monitoring's average and used
// lines) is a neutral stone, and at most one series leans blue.
func TestChartColorsAreNotMostlyBlue(t *testing.T) {
	t.Parallel()
	blue := 0
	for i, c := range chartColors {
		v, err := strconv.ParseUint(strings.TrimPrefix(c, "#"), 16, 32)
		if err != nil {
			t.Fatalf("chart colour %q: %v", c, err)
		}
		if r, g, b := int(v>>16), int(v>>8&0xff), int(v&0xff); b > r && b > g {
			blue++
			if i == 0 {
				t.Errorf("the first series is %s, which leans blue", c)
			}
		}
	}
	if blue > 1 {
		t.Errorf("%d of %d chart colours lean blue; want at most one", blue, len(chartColors))
	}
	for _, want := range []string{`<i style="background: ` + chartColors[0] + `"></i>average`, `<i style="background: ` + chartColors[1] + `"></i>peak`,
		`<i style="background: ` + chartColors[0] + `"></i>used`} {
		if !strings.Contains(hostResourcesHTML, want) {
			t.Errorf("host monitoring's legend lacks %s", want)
		}
	}
}
