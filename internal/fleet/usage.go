package fleet

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/usage"
)

// RecordUsage stores a box's read of its plans' usage, received at at: each login, each window's
// value as a run in usage_readings, which login the box uses, and how each read went.
func (r *Registry) RecordUsage(ctx context.Context, box string, rep usage.Report, at time.Time) error {
	collected := rep.CollectedAt
	if collected.IsZero() {
		collected = at
	}
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		for _, s := range rep.Sources {
			if s.Missing || s.Provider == "" {
				continue
			}
			if err := setSource(ctx, tx, box, s.Provider, "", s.Error, "", collected, at); err != nil {
				return err
			}
			if s.Error != "" {
				continue
			}
			for _, a := range s.Accounts {
				if a.ID == "" {
					continue
				}
				if err := recordAccount(ctx, tx, box, s.Provider, a, collected, at); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func setSource(ctx context.Context, tx pgx.Tx, box, provider, account, errMsg, note string, observed, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO usage_sources (box, provider, account, ok, error, note, observed_at, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (box, provider, account) DO UPDATE SET ok = EXCLUDED.ok, error = EXCLUDED.error, note = EXCLUDED.note,
		observed_at = EXCLUDED.observed_at, received_at = EXCLUDED.received_at`,
		box, provider, account, errMsg == "", errMsg, note, nullTime(observed), at)
	return err
}

func recordAccount(ctx context.Context, tx pgx.Tx, box, provider string, a usage.Account, collected, at time.Time) error {
	obs := a.ObservedAt
	if obs.IsZero() {
		obs = collected
	}
	var slot *int
	if a.Slot > 0 {
		slot = &a.Slot
	}
	// The upsert also locks the login's row, so two boxes reading one login take turns.
	if _, err := tx.Exec(ctx, `INSERT INTO usage_accounts (provider, account, label, plan, slot, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (provider, account) DO UPDATE SET
		label = CASE WHEN EXCLUDED.last_seen >= usage_accounts.last_seen AND EXCLUDED.label <> '' THEN EXCLUDED.label ELSE usage_accounts.label END,
		plan = CASE WHEN EXCLUDED.last_seen >= usage_accounts.last_seen AND EXCLUDED.plan <> '' THEN EXCLUDED.plan ELSE usage_accounts.plan END,
		slot = CASE WHEN EXCLUDED.last_seen >= usage_accounts.last_seen AND EXCLUDED.slot IS NOT NULL THEN EXCLUDED.slot ELSE usage_accounts.slot END,
		first_seen = least(usage_accounts.first_seen, EXCLUDED.first_seen),
		last_seen = greatest(usage_accounts.last_seen, EXCLUDED.last_seen)`,
		provider, a.ID, a.Label, a.Plan, slot, obs); err != nil {
		return err
	}
	if err := setSource(ctx, tx, box, provider, a.ID, a.Error, a.Note, obs, at); err != nil {
		return err
	}
	if a.Active {
		if err := markActive(ctx, tx, box, provider, a.ID, collected); err != nil {
			return err
		}
	}
	if a.Error != "" {
		return nil
	}
	for _, w := range a.Windows {
		if w.ID == "" {
			continue
		}
		if err := recordWindow(ctx, tx, provider, a.ID, w, obs); err != nil {
			return err
		}
	}
	return nil
}

// recordWindow extends the window's newest run when the value holds, or starts a new one.
func recordWindow(ctx context.Context, tx pgx.Tx, provider, account string, w usage.Window, obs time.Time) error {
	var used float64
	var reset *time.Time
	var first, last time.Time
	err := tx.QueryRow(ctx, `SELECT used_pct, resets_at, first_observed, last_observed FROM usage_readings
		WHERE provider = $1 AND account = $2 AND window_id = $3 ORDER BY first_observed DESC LIMIT 1`,
		provider, account, w.ID).Scan(&used, &reset, &first, &last)
	var seconds *int
	if w.Seconds > 0 {
		seconds = &w.Seconds
	}
	newReset := nullTime(w.ResetsAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case !obs.After(last):
		return nil // no newer than what is stored: another box's older cache, or a repeat
	case used == w.UsedPct && (sameTime(reset, newReset) || w.UsedPct == 0):
		// the same value; at 0% a moving reset is a window not yet started
		_, err := tx.Exec(ctx, `UPDATE usage_readings SET last_observed = $5, observations = observations + 1,
			resets_at = $6, window_seconds = coalesce($7, window_seconds)
			WHERE provider = $1 AND account = $2 AND window_id = $3 AND first_observed = $4`,
			provider, account, w.ID, first, obs, newReset, seconds)
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_readings (provider, account, window_id, window_seconds, used_pct, resets_at,
		first_observed, last_observed) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		provider, account, w.ID, seconds, w.UsedPct, newReset, obs)
	return err
}

// markActive extends the box's current active-login run, or starts one when the login changed.
func markActive(ctx context.Context, tx pgx.Tx, box, provider, account string, at time.Time) error {
	var cur string
	var since, until time.Time
	err := tx.QueryRow(ctx, `SELECT account, since, until FROM usage_active WHERE box = $1 AND provider = $2
		ORDER BY since DESC LIMIT 1`, box, provider).Scan(&cur, &since, &until)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case !at.After(until):
		return nil
	case cur == account:
		_, err := tx.Exec(ctx, "UPDATE usage_active SET until = $4 WHERE box = $1 AND provider = $2 AND since = $3",
			box, provider, since, at)
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO usage_active (box, provider, account, since, until) VALUES ($1, $2, $3, $4, $4)",
		box, provider, account, at)
	return err
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// Plan is one login's plan as last read: each window, which boxes use it now, and what is wrong.
type Plan struct {
	Provider string       `json:"provider"`
	Account  string       `json:"account"`
	Label    string       `json:"label"`
	Plan     string       `json:"plan,omitempty"`
	Slot     int          `json:"slot,omitempty"`
	LastSeen time.Time    `json:"lastSeen"`
	ActiveOn []string     `json:"activeOn,omitempty"` // boxes whose tool uses this login now
	Error    string       `json:"error,omitempty"`    // the newest read of it failed
	Note     string       `json:"note,omitempty"`
	Windows  []PlanWindow `json:"windows,omitempty"`
}

// PlanWindow is a window's latest value (usage_current, usage_pace).
type PlanWindow struct {
	ID           string    `json:"id"`
	Seconds      int       `json:"seconds,omitempty"`
	UsedPct      float64   `json:"usedPct"`
	RemainingPct *float64  `json:"remainingPct"` // nil once the window has reset
	ResetsAt     time.Time `json:"resetsAt,omitzero"`
	Elapsed      bool      `json:"elapsed"`
	Stale        bool      `json:"stale"`
	LastObserved time.Time `json:"lastObserved"`
	Pace         *float64  `json:"pace,omitempty"` // % left minus % of the window to run
}

// PlansView is every login's plan, and the boxes whose read of a provider failed lately.
type PlansView struct {
	Plans    []Plan   `json:"plans"`
	Problems []string `json:"problems,omitempty"` // "box: provider: error"
}

// recent is how long a box's view of a login (active, failed) stands without a new read.
const recent = "15 minutes"

var providerOrder = map[string]int{usage.Claude: 0, usage.Codex: 1, usage.Muse: 2, usage.Antigravity: 3}

// Plans is every login, in provider order, then cswap slot.
func (r *Registry) Plans(ctx context.Context) (PlansView, error) {
	var v PlansView
	rows, err := r.p.Query(ctx, `SELECT a.provider, a.account, a.label, a.plan, coalesce(a.slot, 0), a.last_seen,
		coalesce((SELECT array_agg(x.box ORDER BY x.box) FROM (
			SELECT DISTINCT ON (u.box) u.box, u.account, u.until FROM usage_active u
			WHERE u.provider = a.provider ORDER BY u.box, u.since DESC) x
			WHERE x.account = a.account AND x.until > now() - interval '`+recent+`'), '{}'),
		coalesce((SELECT s.error FROM usage_sources s WHERE s.provider = a.provider AND s.account = a.account AND NOT s.ok
			AND s.received_at > now() - interval '`+recent+`' ORDER BY s.received_at DESC LIMIT 1), ''),
		coalesce((SELECT s.note FROM usage_sources s WHERE s.provider = a.provider AND s.account = a.account AND s.ok
			ORDER BY s.received_at DESC LIMIT 1), '')
		FROM usage_accounts a`)
	if err != nil {
		return v, err
	}
	idx := map[[2]string]int{}
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.Provider, &p.Account, &p.Label, &p.Plan, &p.Slot, &p.LastSeen, &p.ActiveOn, &p.Error, &p.Note); err != nil {
			rows.Close()
			return v, err
		}
		v.Plans = append(v.Plans, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return v, err
	}
	sortPlans(v.Plans)
	for i, p := range v.Plans {
		idx[[2]string{p.Provider, p.Account}] = i
	}
	rows, err = r.p.Query(ctx, `SELECT c.provider, c.account, c.window_id, coalesce(c.window_seconds, 0), c.used_pct, c.remaining_pct,
		c.resets_at, c.elapsed, c.stale, c.last_observed, p.pace
		FROM usage_current c LEFT JOIN usage_pace p USING (provider, account, window_id)`)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var prov, acct string
		var w PlanWindow
		var reset *time.Time
		if err := rows.Scan(&prov, &acct, &w.ID, &w.Seconds, &w.UsedPct, &w.RemainingPct, &reset, &w.Elapsed, &w.Stale, &w.LastObserved, &w.Pace); err != nil {
			rows.Close()
			return v, err
		}
		w.ResetsAt = deref(reset)
		if i, ok := idx[[2]string{prov, acct}]; ok {
			v.Plans[i].Windows = append(v.Plans[i].Windows, w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return v, err
	}
	for i := range v.Plans {
		ws := v.Plans[i].Windows
		sort.SliceStable(ws, func(a, b int) bool { return windowRank(ws[a].ID) < windowRank(ws[b].ID) })
	}
	rows, err = r.p.Query(ctx, `SELECT box || ': ' || provider || ': ' || error FROM usage_sources
		WHERE account = '' AND NOT ok AND received_at > now() - interval '`+recent+`' ORDER BY box, provider`)
	if err != nil {
		return v, err
	}
	v.Problems, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return v, err
}

func sortPlans(ps []Plan) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if oa, ob := order(a.Provider), order(b.Provider); oa != ob {
			return oa < ob
		}
		if a.Slot != b.Slot {
			return a.Slot < b.Slot
		}
		return a.Label < b.Label
	})
}

func order(provider string) int {
	if o, ok := providerOrder[provider]; ok {
		return o
	}
	return len(providerOrder)
}

// windowRank puts a login's own windows first, the week before the short one, then pools by name.
func windowRank(id string) string {
	switch id {
	case "7d":
		return "0"
	case "5h":
		return "1"
	}
	return "2" + id
}

// Point is a window's used % at a time.
type Point struct {
	At   time.Time `json:"at"`
	Used float64   `json:"used"`
}

// Series is one login's window over time: each run in the range as a step (its first and last
// reading).
type Series struct {
	Provider string  `json:"provider"`
	Account  string  `json:"account"`
	Window   string  `json:"window"`
	Points   []Point `json:"points"`
}

// UsageHistory is every window seen since since, by login and window.
func (r *Registry) UsageHistory(ctx context.Context, since time.Time) ([]Series, error) {
	rows, err := r.p.Query(ctx, `SELECT provider, account, window_id, used_pct, first_observed, last_observed
		FROM usage_readings WHERE last_observed >= $1 ORDER BY provider, account, window_id, first_observed`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Series
	for rows.Next() {
		var prov, acct, win string
		var used float64
		var first, last time.Time
		if err := rows.Scan(&prov, &acct, &win, &used, &first, &last); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].Provider != prov || out[n-1].Account != acct || out[n-1].Window != win {
			out = append(out, Series{Provider: prov, Account: acct, Window: win})
		}
		s := &out[len(out)-1]
		s.Points = append(s.Points, Point{first.UTC(), used}, Point{last.UTC(), used})
	}
	return out, rows.Err()
}

// Cycle is one week of a login: the most of it used.
type Cycle struct {
	Provider string    `json:"provider"`
	Account  string    `json:"account"`
	Window   string    `json:"window"`
	ResetsAt time.Time `json:"resetsAt"`
	Peak     float64   `json:"peak"`
}

// WeeklyPeaks is each login's weekly windows (7d and pools' /7d) seen since since, newest first.
func (r *Registry) WeeklyPeaks(ctx context.Context, since time.Time) ([]Cycle, error) {
	rows, err := r.p.Query(ctx, `SELECT provider, account, window_id, resets_at, peak_used_pct FROM usage_cycles
		WHERE (window_id = '7d' OR window_id LIKE '%/7d') AND last_observed >= $1
		ORDER BY resets_at DESC, provider, account, window_id`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Cycle, error) {
		var c Cycle
		err := row.Scan(&c.Provider, &c.Account, &c.Window, &c.ResetsAt, &c.Peak)
		c.ResetsAt = c.ResetsAt.UTC()
		return c, err
	})
}
