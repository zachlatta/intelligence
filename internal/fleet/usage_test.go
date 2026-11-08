package fleet

import (
	"math"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/usage"
)

func usageRig(t *testing.T) *Registry { return New(dbtest.New(t).Owner) }

func week(used float64, reset time.Time) usage.Window {
	return usage.Window{ID: "7d", Seconds: usage.Week, UsedPct: used, ResetsAt: reset}
}

func claudeRead(at time.Time, accts ...usage.Account) usage.Report {
	for i := range accts {
		if accts[i].ObservedAt.IsZero() {
			accts[i].ObservedAt = at
		}
	}
	return usage.Report{CollectedAt: at, Sources: []usage.Source{{Provider: usage.Claude, Accounts: accts}}}
}

func ada(active bool, ws ...usage.Window) usage.Account {
	return usage.Account{ID: "ada@example.com", Label: "ada@example.com", Slot: 1, Active: active, Windows: ws}
}

func grace(active bool, ws ...usage.Window) usage.Account {
	return usage.Account{ID: "grace@example.com", Label: "grace@example.com", Slot: 2, Active: active, Windows: ws}
}

func recordUsage(t *testing.T, r *Registry, host string, rep usage.Report) {
	t.Helper()
	if err := r.RecordUsage(ctx, host, rep, rep.CollectedAt); err != nil {
		t.Fatal(err)
	}
}

type run struct {
	Window       string
	Used         float64
	Reset        time.Time
	First, Last  time.Time
	Observations int
}

func runs(t *testing.T, r *Registry, account string) []run {
	t.Helper()
	rows, err := r.p.Query(ctx, `SELECT window_id, used_pct, coalesce(resets_at, 'epoch'), first_observed, last_observed, observations
		FROM usage_readings WHERE account = $1 ORDER BY window_id, first_observed`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []run
	for rows.Next() {
		var x run
		if err := rows.Scan(&x.Window, &x.Used, &x.Reset, &x.First, &x.Last, &x.Observations); err != nil {
			t.Fatal(err)
		}
		x.Reset, x.First, x.Last = x.Reset.UTC(), x.First.UTC(), x.Last.UTC()
		out = append(out, x)
	}
	return out
}

func TestUsageStoresARunPerValue(t *testing.T) {
	r := usageRig(t)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(10, reset))))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(5*time.Minute), ada(true, week(10, reset))))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(10*time.Minute), ada(true, week(12.5, reset))))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(15*time.Minute), ada(true, week(12.5, reset))))
	got := runs(t, r, "ada@example.com")
	want := []run{
		{"7d", 10, reset, t1, t1.Add(5 * time.Minute), 2},
		{"7d", 12.5, reset, t1.Add(10 * time.Minute), t1.Add(15 * time.Minute), 2},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("runs =\n%+v\nwant\n%+v", got, want)
	}
	// a new week starts a new run at the same value
	recordUsage(t, r, "host-1", claudeRead(t1.Add(20*time.Minute), ada(true, week(12.5, reset.Add(usage.Week*time.Second)))))
	if got := runs(t, r, "ada@example.com"); len(got) != 3 {
		t.Errorf("after a reset: %+v", got)
	}
}

func TestUsageIgnoresAReadingOlderThanTheNewest(t *testing.T) {
	r := usageRig(t)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1.Add(10*time.Minute), ada(true, week(20, reset))))
	// another host's cswap cache is older
	recordUsage(t, r, "host-2", claudeRead(t1.Add(11*time.Minute), usage.Account{ID: "ada@example.com", Label: "ada@example.com",
		ObservedAt: t1, Windows: []usage.Window{week(5, reset)}}))
	got := runs(t, r, "ada@example.com")
	if len(got) != 1 || got[0].Used != 20 || got[0].Observations != 1 {
		t.Errorf("runs = %+v", got)
	}
}

func TestUsageOfOneLoginFromTwoHostsIsOneLogin(t *testing.T) {
	r := usageRig(t)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(10, reset))))
	recordUsage(t, r, "host-2", claudeRead(t1.Add(time.Minute), ada(false, week(10, reset))))
	var accounts int
	if err := r.p.QueryRow(ctx, "SELECT count(*) FROM usage_accounts").Scan(&accounts); err != nil || accounts != 1 {
		t.Errorf("accounts = %d, %v", accounts, err)
	}
	if got := runs(t, r, "ada@example.com"); len(got) != 1 || got[0].Observations != 2 {
		t.Errorf("runs = %+v", got)
	}
}

func TestUsageAWindowNotYetStartedIsOneRun(t *testing.T) {
	r := usageRig(t)
	// an unused pool's reset is always a week from now
	for i := range 3 {
		at := t1.Add(time.Duration(i) * 5 * time.Minute)
		recordUsage(t, r, "host-1", claudeRead(at, ada(true, week(0, at.Add(usage.Week*time.Second)))))
	}
	got := runs(t, r, "ada@example.com")
	if len(got) != 1 || got[0].Observations != 3 || !got[0].Reset.Equal(t1.Add(10*time.Minute+usage.Week*time.Second)) {
		t.Errorf("runs = %+v", got)
	}
}

func TestUsageRecordsWhichLoginEachHostUses(t *testing.T) {
	r := usageRig(t)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(1, reset)), grace(false, week(1, reset))))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(5*time.Minute), ada(true, week(1, reset)), grace(false, week(1, reset))))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(10*time.Minute), ada(false, week(1, reset)), grace(true, week(1, reset))))
	rows, err := r.p.Query(ctx, "SELECT account, since, until FROM usage_active WHERE host = 'host-1' ORDER BY since")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type span struct {
		Account      string
		Since, Until time.Time
	}
	var got []span
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.Account, &s.Since, &s.Until); err != nil {
			t.Fatal(err)
		}
		s.Since, s.Until = s.Since.UTC(), s.Until.UTC()
		got = append(got, s)
	}
	want := []span{{"ada@example.com", t1, t1.Add(5 * time.Minute)}, {"grace@example.com", t1.Add(10 * time.Minute), t1.Add(10 * time.Minute)}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("active =\n%+v\nwant\n%+v", got, want)
	}
}

func TestUsageFailuresAreRecordedButNeverRead(t *testing.T) {
	r := usageRig(t)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(10, reset))))
	bad := ada(true)
	bad.Error = "cswap usage status: expired"
	rep := claudeRead(t1.Add(5*time.Minute), bad)
	rep.Sources = append(rep.Sources, usage.Source{Provider: usage.Muse, Error: "muse usage: not_logged_in"},
		usage.Source{Provider: usage.Antigravity, Missing: true})
	recordUsage(t, r, "host-1", rep)
	if got := runs(t, r, "ada@example.com"); len(got) != 1 || got[0].Observations != 1 {
		t.Errorf("runs = %+v", got)
	}
	type src struct {
		Provider, Account string
		OK                bool
		Error             string
	}
	rows, err := r.p.Query(ctx, "SELECT provider, account, ok, error FROM usage_sources ORDER BY provider, account")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []src
	for rows.Next() {
		var s src
		if err := rows.Scan(&s.Provider, &s.Account, &s.OK, &s.Error); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	want := []src{
		{"claude", "", true, ""},
		{"claude", "ada@example.com", false, "cswap usage status: expired"},
		{"muse", "", false, "muse usage: not_logged_in"},
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUsageViews(t *testing.T) {
	r := usageRig(t)
	now := time.Now().UTC().Truncate(time.Minute)
	// ada: 30% used with half the week to go (ahead by 20); grace: read 20 minutes ago (stale);
	// linus: a window that has already reset
	recordUsage(t, r, "host-1", claudeRead(now,
		ada(true, week(30, now.Add(84*time.Hour)), usage.Window{ID: "5h", Seconds: usage.FiveHours, UsedPct: 50, ResetsAt: now.Add(time.Hour)}),
		usage.Account{ID: "linus@example.com", Label: "linus@example.com", ObservedAt: now.Add(-2 * time.Hour),
			Windows: []usage.Window{week(90, now.Add(-time.Hour))}},
		usage.Account{ID: "grace@example.com", Label: "grace@example.com", ObservedAt: now.Add(-20 * time.Minute),
			Windows: []usage.Window{week(40, now.Add(100*time.Hour))}}))
	type cur struct {
		Remaining      *float64
		Elapsed, Stale bool
	}
	current := map[string]cur{}
	rows, err := r.p.Query(ctx, "SELECT account || ' ' || window_id, remaining_pct, elapsed, stale FROM usage_current")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var c cur
		if err := rows.Scan(&k, &c.Remaining, &c.Elapsed, &c.Stale); err != nil {
			t.Fatal(err)
		}
		current[k] = c
	}
	rows.Close()
	if c := current["ada@example.com 7d"]; c.Remaining == nil || *c.Remaining != 70 || c.Elapsed || c.Stale {
		t.Errorf("ada 7d = %+v", c)
	}
	if c := current["linus@example.com 7d"]; c.Remaining != nil || !c.Elapsed {
		t.Errorf("linus 7d = %+v", c)
	}
	if c := current["grace@example.com 7d"]; c.Remaining == nil || !c.Stale {
		t.Errorf("grace 7d = %+v", c)
	}
	var pace float64
	if err := r.p.QueryRow(ctx, "SELECT pace FROM usage_pace WHERE account = 'ada@example.com' AND window_id = '7d'").Scan(&pace); err != nil {
		t.Fatal(err)
	}
	if math.Abs(pace-20) > 0.5 {
		t.Errorf("ada pace = %v, want about 20", pace)
	}
	var n int
	if err := r.p.QueryRow(ctx, "SELECT count(*) FROM usage_pace WHERE account = 'linus@example.com'").Scan(&n); err != nil || n != 0 {
		t.Errorf("elapsed window has pace: %d %v", n, err)
	}
	var logins, read int
	var weeks float64
	if err := r.p.QueryRow(ctx, "SELECT logins, logins_read, weeks_left FROM usage_totals WHERE provider = 'claude'").Scan(&logins, &read, &weeks); err != nil {
		t.Fatal(err)
	}
	if logins != 3 || read != 1 || weeks != 0.7 {
		t.Errorf("totals = %d logins, %d read, %v weeks", logins, read, weeks)
	}
	var peak float64
	recordUsage(t, r, "host-1", claudeRead(now.Add(time.Minute), ada(true, week(35, now.Add(84*time.Hour)))))
	if err := r.p.QueryRow(ctx, "SELECT peak_used_pct FROM usage_cycles WHERE account = 'ada@example.com' AND window_id = '7d'").Scan(&peak); err != nil || peak != 35 {
		t.Errorf("cycle peak = %v, %v", peak, err)
	}
}

func TestPlansShowEachLoginsWindowsWhereItIsActiveAndItsProblems(t *testing.T) {
	r := usageRig(t)
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(84 * time.Hour)
	g := grace(false, week(0, now.Add(150*time.Hour)))
	g.Error = "cswap usage status: expired"
	rep := claudeRead(now, ada(true, usage.Window{ID: "fable/7d", Seconds: usage.Week, UsedPct: 1, ResetsAt: reset},
		usage.Window{ID: "5h", Seconds: usage.FiveHours, UsedPct: 50, ResetsAt: now.Add(time.Hour)}, week(30, reset)), g)
	rep.Sources = append(rep.Sources,
		usage.Source{Provider: usage.Muse, Accounts: []usage.Account{{ID: "default", Label: "Muse", Plan: "Power", Active: true, ObservedAt: now,
			Note: "no usage reported yet in this window"}}},
		usage.Source{Provider: usage.Antigravity, Error: "agy /usage: status ERROR"})
	recordUsage(t, r, "host-1", rep)
	recordUsage(t, r, "host-2", claudeRead(now, grace(true)))
	v, err := r.Plans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Plans) != 3 {
		t.Fatalf("plans = %+v", v.Plans)
	}
	a, gr, m := v.Plans[0], v.Plans[1], v.Plans[2]
	if a.Account != "ada@example.com" || a.Slot != 1 || len(a.ActiveOn) != 1 || a.ActiveOn[0] != "host-1" {
		t.Errorf("ada = %+v", a)
	}
	var ids []string
	for _, w := range a.Windows {
		ids = append(ids, w.ID)
	}
	if len(ids) != 3 || ids[0] != "7d" || ids[1] != "5h" || ids[2] != "fable/7d" {
		t.Errorf("ada's windows in order = %v", ids)
	}
	if w := a.Windows[0]; w.RemainingPct == nil || *w.RemainingPct != 70 || w.Pace == nil || math.Abs(*w.Pace-20) > 0.5 || !w.ResetsAt.Equal(reset) {
		t.Errorf("ada 7d = %+v", w)
	}
	if gr.Account != "grace@example.com" || gr.Error != "cswap usage status: expired" || len(gr.Windows) != 0 ||
		len(gr.ActiveOn) != 1 || gr.ActiveOn[0] != "host-2" {
		t.Errorf("grace = %+v", gr)
	}
	if m.Provider != usage.Muse || m.Note == "" || m.Plan != "Power" {
		t.Errorf("muse = %+v", m)
	}
	if len(v.Problems) != 1 || v.Problems[0] != "host-1: antigravity: agy /usage: status ERROR" {
		t.Errorf("problems = %q", v.Problems)
	}
}

func TestUsageHistoryAndWeeklyPeaks(t *testing.T) {
	r := usageRig(t)
	now := time.Now().UTC().Truncate(time.Second)
	reset1, reset2 := now.Add(-48*time.Hour), now.Add(5*24*time.Hour)
	recordUsage(t, r, "host-1", claudeRead(now.Add(-72*time.Hour), ada(true, week(40, reset1))))
	recordUsage(t, r, "host-1", claudeRead(now.Add(-50*time.Hour), ada(true, week(95, reset1))))
	recordUsage(t, r, "host-1", claudeRead(now.Add(-time.Hour), ada(true, week(10, reset2))))
	recordUsage(t, r, "host-1", claudeRead(now, ada(true, week(10, reset2))))
	hist, err := r.UsageHistory(ctx, now.Add(-60*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Account != "ada@example.com" || hist[0].Window != "7d" {
		t.Fatalf("history = %+v", hist)
	}
	// each run in the range is a step: its first and last reading
	pts := hist[0].Points
	want := []Point{{now.Add(-50 * time.Hour), 95}, {now.Add(-50 * time.Hour), 95}, {now.Add(-time.Hour), 10}, {now, 10}}
	if len(pts) != len(want) {
		t.Fatalf("points = %+v", pts)
	}
	for i := range want {
		if !pts[i].At.Equal(want[i].At) || pts[i].Used != want[i].Used {
			t.Errorf("point %d = %+v, want %+v", i, pts[i], want[i])
		}
	}
	peaks, err := r.WeeklyPeaks(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(peaks) != 2 || peaks[0].Peak != 10 || peaks[1].Peak != 95 || !peaks[1].ResetsAt.Equal(reset1) {
		t.Errorf("peaks = %+v", peaks)
	}
}
