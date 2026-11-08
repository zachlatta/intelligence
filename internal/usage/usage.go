// Package usage reads how much of each subscription plan's quota is used, per login, from each
// provider's own meter: every Claude login cswap holds (`cswap list --json`), Codex's subscription
// meter (with the login's own token, falling back to the newest local session reading), Muse
// (`muse usage --json`) and Antigravity (`agy -p /usage`). A dev box reads them and sends the
// Report up its stream; the server keeps the history.
package usage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Providers.
const (
	Claude      = "claude"
	Codex       = "codex"
	Muse        = "muse"
	Antigravity = "antigravity"
)

// Window lengths, in seconds.
const (
	Week      = 7 * 24 * 3600
	FiveHours = 5 * 3600
)

// Report is one read of every provider on a box.
type Report struct {
	CollectedAt time.Time `json:"collectedAt"`
	Sources     []Source  `json:"sources"`
}

// Source is one provider's read.
type Source struct {
	Provider string    `json:"provider"`
	Missing  bool      `json:"missing,omitempty"` // the box has no such tool or login: nothing to read
	Error    string    `json:"error,omitempty"`   // the read failed; Accounts is empty
	Accounts []Account `json:"accounts,omitempty"`
}

// Account is one login's plan and how much of each of its quota windows is used.
type Account struct {
	ID         string    `json:"id"`              // stable within the provider: the login's email, or "default"
	Label      string    `json:"label"`           // what to show for it
	Plan       string    `json:"plan,omitempty"`  // the plan or tier, where the provider says
	Slot       int       `json:"slot,omitempty"`  // cswap's account number on this box; it can change
	Active     bool      `json:"active"`          // the login this box's tool uses now
	ObservedAt time.Time `json:"observedAt"`      // when the provider was read (cswap caches, so not now)
	Note       string    `json:"note,omitempty"`  // a caveat on the reading
	Error      string    `json:"error,omitempty"` // this login's read failed; Windows is empty
	Windows    []Window  `json:"windows,omitempty"`
}

// Window is one quota window: "7d", "5h", or a pool's window such as "fable/7d" or "gemini/5h".
type Window struct {
	ID       string    `json:"id"`
	Seconds  int       `json:"seconds,omitempty"` // its length; 0 unknown
	UsedPct  float64   `json:"usedPct"`
	ResetsAt time.Time `json:"resetsAt,omitzero"` // rounded to the minute: providers jitter it by up to a second
}

// ErrMissing: the tool is not installed on this box.
var ErrMissing = errors.New("not installed")

// Reader reads every provider on this box.
type Reader struct {
	Home     string // the user's home: Codex's login, and ~/.local/bin for the tools
	CodexURL string // Codex's subscription meter; default the real one
	HTTP     *http.Client
	Now      func() time.Time
	// Run runs a tool and returns its stdout; ErrMissing when it is not installed. Default: run
	// it from PATH or ~/.local/bin, in Home.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Timeout bounds each provider's read; default 25s.
	Timeout time.Duration
}

const codexMeter = "https://chatgpt.com/backend-api/wham/usage"

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Read reads every provider at once and returns them in a fixed order.
func (r *Reader) Read(ctx context.Context) Report {
	rep := Report{CollectedAt: r.now()}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 25 * time.Second
	}
	reads := []func(context.Context) Source{r.readClaude, r.readCodex, r.readMuse, r.readAntigravity}
	rep.Sources = make([]Source, len(reads))
	var wg sync.WaitGroup
	for i, read := range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			rep.Sources[i] = read(ctx)
		}()
	}
	wg.Wait()
	return rep
}

func (r *Reader) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.Run != nil {
		return r.Run(ctx, name, args...)
	}
	path, err := exec.LookPath(name)
	if err != nil && r.Home != "" {
		path, err = exec.LookPath(filepath.Join(r.Home, ".local/bin", name))
	}
	if err != nil {
		return nil, ErrMissing
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = r.Home
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, clip(msg, 300))
		}
		return out, err
	}
	return out, nil
}

// readTool runs a tool and parses its output, or says why it could not.
func (r *Reader) readTool(ctx context.Context, provider string, parse func([]byte, time.Time) Source, name string, args ...string) Source {
	out, err := r.run(ctx, name, args...)
	switch {
	case errors.Is(err, ErrMissing):
		return Source{Provider: provider, Missing: true}
	case err != nil:
		return Source{Provider: provider, Error: fmt.Sprintf("%s: %v", name, err)}
	}
	return parse(out, r.now())
}

func (r *Reader) readClaude(ctx context.Context) Source {
	return r.readTool(ctx, Claude, parseClaude, "cswap", "list", "--json")
}

func (r *Reader) readMuse(ctx context.Context) Source {
	return r.readTool(ctx, Muse, parseMuse, "muse", "usage", "--json")
}

// readAntigravity runs a read-only slash command in print mode: no agent turn, no quota spent.
func (r *Reader) readAntigravity(ctx context.Context) Source {
	return r.readTool(ctx, Antigravity, parseAntigravity, "agy", "-p", "/usage", "--output-format", "json")
}

func parseClaude(raw []byte, now time.Time) Source {
	type win struct {
		Pct      *float64 `json:"pct"`
		ResetsAt string   `json:"resetsAt"`
		Name     string   `json:"name"`
	}
	var payload struct {
		Accounts []struct {
			Number         int    `json:"number"`
			Email          string `json:"email"`
			Active         bool   `json:"active"`
			Org            string `json:"organizationUuid"`
			UsageStatus    string `json:"usageStatus"`
			UsageFetchedAt string `json:"usageFetchedAt"`
			Usage          *struct {
				SevenDay *win  `json:"sevenDay"`
				FiveHour *win  `json:"fiveHour"`
				Scoped   []win `json:"scoped"`
			} `json:"usage"`
		} `json:"accounts"`
	}
	s := Source{Provider: Claude}
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.Error = "cswap list: unreadable output: " + err.Error()
		return s
	}
	if len(payload.Accounts) == 0 {
		s.Error = "cswap list: no logins"
		return s
	}
	emails := map[string]int{}
	for _, a := range payload.Accounts {
		emails[strings.ToLower(a.Email)]++
	}
	for _, a := range payload.Accounts {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		acct := Account{ID: email, Label: email, Slot: a.Number, Active: a.Active, ObservedAt: now}
		switch {
		case email == "":
			acct.ID, acct.Label = fmt.Sprintf("slot-%d", a.Number), fmt.Sprintf("Claude #%d", a.Number)
		case emails[email] > 1: // one email in two organizations: two logins, two plans
			acct.ID = email + "#" + hash(a.Org)[:8]
		}
		if t, ok := parseTime(a.UsageFetchedAt); ok {
			acct.ObservedAt = t
		}
		add := func(id string, seconds int, w *win) {
			if w != nil && w.Pct != nil {
				acct.Windows = append(acct.Windows, Window{ID: id, Seconds: seconds, UsedPct: *w.Pct, ResetsAt: resetTime(w.ResetsAt)})
			}
		}
		if a.UsageStatus != "ok" || a.Usage == nil {
			acct.Error = "cswap usage status: " + or(a.UsageStatus, "unknown")
		} else {
			add("7d", Week, a.Usage.SevenDay)
			add("5h", FiveHours, a.Usage.FiveHour)
			for _, w := range a.Usage.Scoped {
				add(slug(w.Name)+"/7d", Week, &w)
			}
		}
		s.Accounts = append(s.Accounts, acct)
	}
	sort.SliceStable(s.Accounts, func(i, j int) bool { return s.Accounts[i].Slot < s.Accounts[j].Slot })
	return s
}

// readCodex reads Codex's subscription meter with the login's own token, sent only to the meter.
// When the meter cannot be read it falls back to the newest reading in the local sessions.
func (r *Reader) readCodex(ctx context.Context) Source {
	s := Source{Provider: Codex}
	b, err := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		s.Missing = true
		return s
	}
	if err != nil {
		s.Error = "codex login: " + err.Error()
		return s
	}
	var auth struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Account string `json:"account_id"`
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil || auth.Tokens.Access == "" {
		s.Missing = true // signed in with an API key, or not at all: no subscription to read
		return s
	}
	acct, err := r.codexMeter(ctx, auth.Tokens.Access, auth.Tokens.Account)
	if err == nil {
		s.Accounts = []Account{acct}
		return s
	}
	local, ok := newestCodexSessionReading(r.Home)
	if !ok {
		s.Error = "codex meter: " + err.Error()
		return s
	}
	email, plan := idTokenClaims(auth.Tokens.IDToken)
	local.ID, local.Label, local.Plan = codexID(email, auth.Tokens.Account), or(email, "Codex"), or(plan, local.Plan)
	local.Note = "live meter unavailable (" + err.Error() + "); last local session reading"
	s.Accounts = []Account{local}
	return s
}

func (r *Reader) codexMeter(ctx context.Context, token, account string) (Account, error) {
	url := r.CodexURL
	if url == "" {
		url = codexMeter
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second,
			// never send the token anywhere but the meter
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", account)
	res, err := client.Do(req)
	if err != nil {
		return Account{}, errors.New("unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Account{}, errors.New(res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Account{}, err
	}
	return parseCodexMeter(body, r.now())
}

func parseCodexMeter(body []byte, now time.Time) (Account, error) {
	type win struct {
		Used    *float64 `json:"used_percent"`
		Seconds int      `json:"limit_window_seconds"`
		Reset   int64    `json:"reset_at"`
	}
	var d struct {
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
		Plan      string `json:"plan_type"`
		Rate      struct {
			Primary   *win `json:"primary_window"`
			Secondary *win `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return Account{}, fmt.Errorf("unreadable meter: %w", err)
	}
	email := strings.ToLower(strings.TrimSpace(d.Email))
	a := Account{ID: codexID(email, d.AccountID), Label: or(email, "Codex"), Plan: d.Plan, Active: true, ObservedAt: now}
	for _, w := range []*win{d.Rate.Primary, d.Rate.Secondary} {
		if w == nil || w.Used == nil {
			continue
		}
		a.Windows = append(a.Windows, Window{ID: windowID(w.Seconds), Seconds: w.Seconds, UsedPct: *w.Used, ResetsAt: unixReset(w.Reset)})
	}
	if len(a.Windows) == 0 {
		return Account{}, errors.New("meter has no windows")
	}
	return a, nil
}

func codexID(email, account string) string {
	if email != "" {
		return email
	}
	return "account-" + hash(account)[:12]
}

// newestCodexSessionReading is the last rate-limit reading in the newest Codex session file.
func newestCodexSessionReading(home string) (Account, bool) {
	paths, _ := filepath.Glob(filepath.Join(home, ".codex/sessions/*/*/*/*.jsonl"))
	var newest string
	var newestMod time.Time
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(newestMod) {
			newest, newestMod = p, st.ModTime()
		}
	}
	if newest == "" {
		return Account{}, false
	}
	f, err := os.Open(newest)
	if err != nil {
		return Account{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 4<<20 { // the last 4 MB is plenty
		_, _ = f.Seek(st.Size()-4<<20, io.SeekStart)
	}
	type win struct {
		Used    *float64 `json:"used_percent"`
		Minutes int      `json:"window_minutes"`
		Reset   int64    `json:"resets_at"`
	}
	type line struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			RateLimits *struct {
				Primary   *win   `json:"primary"`
				Secondary *win   `json:"secondary"`
				Plan      string `json:"plan_type"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	var last *line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"rate_limits"`)) {
			continue
		}
		var l line
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Payload.RateLimits != nil && l.Payload.RateLimits.Primary != nil {
			last = &l
		}
	}
	if last == nil {
		return Account{}, false
	}
	t, ok := parseTime(last.Timestamp)
	if !ok {
		return Account{}, false
	}
	a := Account{Active: true, ObservedAt: t, Plan: last.Payload.RateLimits.Plan}
	for _, w := range []*win{last.Payload.RateLimits.Primary, last.Payload.RateLimits.Secondary} {
		if w == nil || w.Used == nil {
			continue
		}
		a.Windows = append(a.Windows, Window{ID: windowID(w.Minutes * 60), Seconds: w.Minutes * 60, UsedPct: *w.Used, ResetsAt: unixReset(w.Reset)})
	}
	return a, len(a.Windows) > 0
}

// idTokenClaims reads the email and plan from Codex's id_token. It is not verified: it only
// names the login the local session reading belongs to.
func idTokenClaims(tok string) (email, plan string) {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return "", ""
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", ""
	}
	var c struct {
		Email string `json:"email"`
		Auth  struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(b, &c) != nil {
		return "", ""
	}
	return strings.ToLower(c.Email), c.Auth.Plan
}

func parseMuse(raw []byte, now time.Time) Source {
	type win struct {
		ResetsAtMs  int64    `json:"resetsAtMs"`
		UsedPercent *float64 `json:"usedPercent"`
	}
	var payload struct {
		Reason    string `json:"reason"`
		Hint      string `json:"hint"`
		Allowance *struct {
			AsOfMs int64  `json:"asOfMs"`
			Tier   string `json:"tier"`
			Weekly *win   `json:"weekly"`
			Window *win   `json:"window"`
		} `json:"allowance"`
	}
	s := Source{Provider: Muse}
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.Error = "muse usage: unreadable output: " + err.Error()
		return s
	}
	acct := Account{ID: "default", Label: "Muse", Active: true, ObservedAt: now}
	if payload.Allowance == nil {
		// Before the first use in a new window an active subscription reports no usage at all.
		// That is not a reading: nothing is stored as used or left.
		if payload.Reason == "server_sent_no_usage" {
			acct.Note = "no usage reported yet in this window"
			s.Accounts = []Account{acct}
			return s
		}
		s.Error = "muse usage: " + or(payload.Reason, payload.Hint, "no allowance")
		return s
	}
	al := payload.Allowance
	acct.Plan = al.Tier
	if al.AsOfMs > 0 {
		acct.ObservedAt = time.UnixMilli(al.AsOfMs).UTC()
	}
	for _, w := range []struct {
		id      string
		seconds int
		w       *win
	}{{"7d", Week, al.Weekly}, {"5h", FiveHours, al.Window}} {
		if w.w == nil || w.w.UsedPercent == nil {
			continue
		}
		var reset time.Time
		if w.w.ResetsAtMs > 0 {
			reset = time.UnixMilli(w.w.ResetsAtMs).UTC().Round(time.Minute)
		}
		acct.Windows = append(acct.Windows, Window{ID: w.id, Seconds: w.seconds, UsedPct: *w.w.UsedPercent, ResetsAt: reset})
	}
	s.Accounts = []Account{acct}
	return s
}

func parseAntigravity(raw []byte, now time.Time) Source {
	var payload struct {
		Status  string `json:"status"`
		Command struct {
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Window    string   `json:"window"`
						Remaining *float64 `json:"remaining_fraction"`
						ResetTime string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	s := Source{Provider: Antigravity}
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.Error = "agy /usage: unreadable output: " + err.Error()
		return s
	}
	if payload.Status != "SUCCESS" {
		s.Error = "agy /usage: status " + or(payload.Status, "missing")
		return s
	}
	acct := Account{ID: "default", Label: "Antigravity", Active: true, ObservedAt: now}
	for _, g := range payload.Command.Data.Groups {
		pool := slug(strings.TrimSuffix(strings.ToLower(g.Name), " models"))
		for _, b := range g.Buckets {
			if b.Remaining == nil {
				continue
			}
			id, seconds := b.Window, 0
			switch b.Window {
			case "weekly":
				id, seconds = "7d", Week
			case "5h":
				seconds = FiveHours
			}
			used := (1 - *b.Remaining) * 100
			acct.Windows = append(acct.Windows, Window{ID: pool + "/" + id, Seconds: seconds, UsedPct: roundTo(used, 2), ResetsAt: resetTime(b.ResetTime)})
		}
	}
	if len(acct.Windows) == 0 {
		s.Error = "agy /usage: no quota buckets"
		return s
	}
	s.Accounts = []Account{acct}
	return s
}

func windowID(seconds int) string {
	switch {
	case seconds == Week:
		return "7d"
	case seconds > 0 && seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds > 0 && seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds > 0:
		return fmt.Sprintf("%ds", seconds)
	}
	return "window"
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func resetTime(s string) time.Time {
	t, _ := parseTime(s)
	return t.Round(time.Minute)
}

func unixReset(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC().Round(time.Minute)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug makes a pool name an id: "Claude and GPT" -> "claude-gpt".
func slug(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), " and ", " ")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func roundTo(f float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(f*p+0.5)) / p
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
