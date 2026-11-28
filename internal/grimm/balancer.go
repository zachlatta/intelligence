package grimm

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// The balancer picks a plan for each hire from Intelligence's own: the account's model groups,
// scored on the session's host by model_group_candidates (headroom against time to reset, with
// the same eligibility gates workflows get). grimm_quota adds Grimm's spread penalty for the
// session's live workers on the same login and window, and Grimm's role policy below decides
// which entries a role may use.

// disabledProviders are never picked, whatever the plan settings say.
var disabledProviders = []string{"gemini", "muse", "copilot", "opencode", "pi", "omp"}

// DisabledProviders is a copy of disabledProviders, for the pages that offer models.
func DisabledProviders() []string { return slices.Clone(disabledProviders) }

// Candidate is one plan entry the balancer can hire on: a grimm_quota row, judged for a role.
type Candidate struct {
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Thinking      string     `json:"thinking"`
	Login         string     `json:"login"`
	LongWindow    string     `json:"longWindow"`
	RemainingPct  *float64   `json:"remainingPct"`
	ExpectedPct   *float64   `json:"expectedRemainingPct"`
	ResetsAt      *time.Time `json:"resetsAt"`
	RawScore      *float64   `json:"rawScore"`
	SpreadPenalty float64    `json:"spreadPenalty"`
	Score         *float64   `json:"score"`
	LiveWorkers   int        `json:"liveWorkers"`
	Eligible      bool       `json:"eligible"`
	Reason        string     `json:"reason"`
}

// Spec is provider/model, as `paseo run --provider` takes it.
func (c Candidate) Spec() string { return c.Provider + "/" + c.Model }

// Mode is the never-ask mode the provider's agents run in.
func (c Candidate) Mode() string { return paseo.UnattendedMode(c.Provider) }

// Policy is a role's rule for which plans it may run on.
type Policy struct {
	Providers   []string          // allowed providers; nil allows any
	Pinned      map[string]string // provider -> the only model the role runs on it
	Preferred   string            // provider ranked first among eligible ones
	DenyVendors []string          // vendors the role never runs on
	// DisjointFrom: roles whose live workers' vendors this role avoids (Soft: unless nothing else is eligible)
	DisjointFrom []string
	Soft         bool
	Reason       string
}

var opusOnly = Policy{Providers: []string{OpusProvider}, Pinned: map[string]string{OpusProvider: OpusModel}, Preferred: OpusProvider,
	DenyVendors: []string{"google"}, Reason: "reviewers and checkers run on claude/claude-opus-5-5 only"}

var policies = map[string]Policy{
	"manager":    {Providers: []string{ManagerProvider}, Pinned: map[string]string{ManagerProvider: ManagerModel}, Preferred: ManagerProvider, Reason: "the manager defaults to claude/claude-opus-5-5"},
	"reviewer":   opusOnly,
	"checker":    opusOnly,
	"audit":      opusOnly,
	"auditor":    opusOnly,
	"verifier":   {Providers: []string{OpusProvider, "codex"}, Pinned: map[string]string{OpusProvider: OpusModel}, Preferred: OpusProvider, DenyVendors: []string{"google"}, DisjointFrom: []string{"builder"}, Reason: "verifiers prefer Opus 5.5, never run on Gemini, and never share the builder's vendor"},
	"builder":    {DisjointFrom: []string{"verifier"}, Soft: true, Reason: "a builder avoids a live verifier's vendor when it can"},
	"researcher": {Reason: "researchers run on any eligible non-disabled plan"},
	"analyst":    {Reason: "analysts run on any eligible non-disabled plan"},
	"prototyper": {Reason: "prototypers run on any eligible non-disabled plan"},
}

// PolicyFor is role's policy; an unknown role has none.
func PolicyFor(role string) Policy { return policies[strings.ToLower(strings.TrimSpace(role))] }

// Vendor is who makes the model an entry runs: a Claude model through Antigravity is Anthropic's.
func Vendor(provider, model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "gemini"):
		return "google"
	case strings.Contains(m, "gpt"):
		return "openai"
	}
	switch provider {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "antigravity", "gemini":
		return "google"
	}
	return provider
}

// quota reads the session's candidates, best first, as the Quota panel shows them.
func quota(ctx context.Context, q pgx.Tx, session string) ([]Candidate, error) {
	rows, err := q.Query(ctx, `SELECT provider, model, thinking, login, long_window, remaining_pct, expected_remaining_pct,
		resets_at, raw_score, spread_penalty::float8, score, live_workers, eligible, reason
		FROM grimm_quota WHERE session_id = $1
		ORDER BY eligible DESC, score DESC NULLS LAST, provider, model, login`, session)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := r.Scan(&c.Provider, &c.Model, &c.Thinking, &c.Login, &c.LongWindow, &c.RemainingPct, &c.ExpectedPct, &c.ResetsAt,
			&c.RawScore, &c.SpreadPenalty, &c.Score, &c.LiveWorkers, &c.Eligible, &c.Reason)
		return c, err
	})
}

// Judge applies role's policy to candidates: it marks what the role may not use (with why),
// avoiding avoidVendors, and ranks the rest. The input is not changed.
func Judge(role string, cands []Candidate, avoidVendors []string) []Candidate {
	p := PolicyFor(role)
	out := make([]Candidate, len(cands))
	copy(out, cands)
	for i := range out {
		c := &out[i]
		if !c.Eligible {
			continue
		}
		vendor := Vendor(c.Provider, c.Model)
		why := ""
		switch {
		case slices.Contains(disabledProviders, c.Provider):
			why = "Provider disabled"
		case p.Providers != nil && !slices.Contains(p.Providers, c.Provider):
			why = p.Reason
		case p.Pinned[c.Provider] != "" && p.Pinned[c.Provider] != c.Model:
			why = p.Reason
		case slices.Contains(p.DenyVendors, vendor):
			why = p.Reason
		case slices.Contains(avoidVendors, vendor):
			why = p.Reason
		}
		if why != "" {
			c.Eligible, c.Reason = false, why
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if pa, pb := a.Provider == p.Preferred && p.Preferred != "", b.Provider == p.Preferred && p.Preferred != ""; pa != pb {
			return pa
		}
		sa, sb := score(a.Score), score(b.Score)
		if sa != sb {
			return sa > sb
		}
		return score(a.RemainingPct) > score(b.RemainingPct)
	})
	return out
}

func score(p *float64) float64 {
	if p == nil {
		return -1e9
	}
	return *p
}

// pick chooses the plan a role's hire runs on in session s, under s's lock: prefer, if set, is a
// provider or provider/model the hire asked for.
func pick(ctx context.Context, tx pgx.Tx, s Session, role, prefer string) (Candidate, error) {
	cands, err := quota(ctx, tx, s.ID)
	if err != nil {
		return Candidate{}, err
	}
	if len(cands) == 0 {
		return Candidate{}, conflict("no plan to hire on: %s has no model group entries (add models to a model group on Plans)", s.Host)
	}
	p := PolicyFor(role)
	var avoid []string
	if len(p.DisjointFrom) > 0 {
		rows, err := tx.Query(ctx, `SELECT provider, model FROM grimm_workers WHERE session_id = $1
			AND status IN ('starting', 'running', 'idle') AND lower(role) = ANY($2)`, s.ID, p.DisjointFrom)
		if err != nil {
			return Candidate{}, err
		}
		for rows.Next() {
			var prov, model string
			if err := rows.Scan(&prov, &model); err != nil {
				rows.Close()
				return Candidate{}, err
			}
			avoid = append(avoid, Vendor(prov, model))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Candidate{}, err
		}
	}
	judged := Judge(role, cands, avoid)
	if p.Soft && len(avoid) > 0 && !anyEligible(judged) {
		judged = Judge(role, cands, nil)
	}
	if prefer = strings.ToLower(strings.TrimSpace(prefer)); prefer != "" {
		prov, model, _ := strings.Cut(prefer, "/")
		if slices.Contains(disabledProviders, prov) {
			return Candidate{}, badRequest("provider %s is disabled", prov)
		}
		if p.Providers != nil && !slices.Contains(p.Providers, prov) {
			return Candidate{}, badRequest("role %s refused provider %s: %s", role, prefer, p.Reason)
		}
		if model != "" && p.Pinned[prov] != "" && p.Pinned[prov] != model {
			return Candidate{}, badRequest("role %s on %s runs on %s only (refused %s)", role, prov, p.Pinned[prov], model)
		}
		var reasons []string
		for _, c := range judged {
			if c.Provider != prov || (model != "" && c.Model != model) {
				continue
			}
			if c.Eligible {
				return c, nil
			}
			reasons = append(reasons, c.Model+" on "+c.Login+": "+c.Reason)
		}
		if len(reasons) == 0 {
			return Candidate{}, conflict("no model group entry on %s for %s", s.Host, prefer)
		}
		return Candidate{}, conflict("%s is not eligible on %s (%s)", prefer, s.Host, strings.Join(reasons, "; "))
	}
	for _, c := range judged {
		if c.Eligible {
			return c, nil
		}
	}
	var reasons []string
	for _, c := range judged {
		reasons = append(reasons, c.Spec()+" on "+c.Login+": "+c.Reason)
		if len(reasons) == 4 {
			break
		}
	}
	return Candidate{}, conflict("no eligible plan for a %s on %s (%s)", role, s.Host, strings.Join(reasons, "; "))
}

func anyEligible(cs []Candidate) bool {
	for _, c := range cs {
		if c.Eligible {
			return true
		}
	}
	return false
}

// pickManager chooses the plan for session s's manager. With no explicit manager override on the
// session it uses the default "manager" role policy (Claude Opus 5.5). With a session-level
// ManagerProvider/ManagerModel override it prefers an eligible grimm_quota entry for that model
// (falling back to the host's reported provider_models catalog when the model is not in a model
// group, so any available non-disabled model can be tried).
func pickManager(ctx context.Context, tx pgx.Tx, s Session) (Candidate, error) {
	prov := strings.ToLower(strings.TrimSpace(s.ManagerProvider))
	model := strings.TrimSpace(s.ManagerModel)
	thinking := strings.TrimSpace(s.ManagerThinking)
	if prov == "" && model == "" {
		c, err := pick(ctx, tx, s, "manager", "")
		if err != nil {
			return Candidate{}, err
		}
		if thinking != "" && c.Provider != "antigravity" {
			c.Thinking = thinking
		}
		return c, nil
	}
	if slices.Contains(disabledProviders, prov) {
		return Candidate{}, badRequest("provider %s is disabled", prov)
	}
	cands, err := quota(ctx, tx, s.ID)
	if err != nil {
		return Candidate{}, err
	}
	var matching []Candidate
	for _, c := range cands {
		if c.Provider == prov && c.Model == model {
			matching = append(matching, c)
		}
	}
	if len(matching) > 0 {
		// Prefer an eligible entry matching the requested thinking level, then any eligible entry for provider/model.
		if thinking != "" {
			for _, c := range matching {
				if c.Eligible && c.Thinking == thinking {
					return c, nil
				}
			}
		}
		for _, c := range matching {
			if c.Eligible {
				if thinking != "" && c.Provider != "antigravity" {
					c.Thinking = thinking
				}
				return c, nil
			}
		}
		var reasons []string
		for _, c := range matching {
			reasons = append(reasons, c.Model+" on "+c.Login+": "+c.Reason)
			if len(reasons) == 4 {
				break
			}
		}
		return Candidate{}, conflict("%s/%s is not eligible on %s (%s)", prov, model, s.Host, strings.Join(reasons, "; "))
	}
	// Not in a model group: allow if the host reports the model in its catalog.
	var available bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM provider_models WHERE host = $1 AND provider = $2 AND id = $3
	)`, s.Host, prov, model).Scan(&available); err != nil {
		return Candidate{}, err
	}
	if !available {
		return Candidate{}, conflict("%s/%s is not available on %s", prov, model, s.Host)
	}
	if prov == "antigravity" {
		thinking = ""
	}
	return Candidate{Provider: prov, Model: model, Thinking: thinking, Eligible: true}, nil
}

// QuotaTable is the candidates as `grimm ctl quota` prints them.
func QuotaTable(cs []Candidate) string {
	if len(cs) == 0 {
		return "No plans: add models to a model group on Plans."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-36s %-18s %-12s %6s %6s %5s %7s  %s\n", "PROVIDER/MODEL", "LOGIN", "POOL", "REM%", "EXP%", "PEN", "SCORE", "STATUS")
	next := false
	for _, c := range cs {
		status := "ELIGIBLE"
		if !c.Eligible {
			status = "skip: " + c.Reason
		} else if !next {
			status, next = "NEXT HIRE", true
		}
		fmt.Fprintf(&b, "%-36s %-18s %-12s %6s %6s %5.0f %7s  %s\n", c.Spec(), cut(c.Login, 18), c.LongWindow, pct(c.RemainingPct),
			pct(c.ExpectedPct), c.SpreadPenalty, signed(c.Score), status)
	}
	return strings.TrimRight(b.String(), "\n")
}

func pct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func signed(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f", *p)
}
