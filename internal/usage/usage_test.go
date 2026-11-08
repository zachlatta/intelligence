package usage

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 8, 19, 30, 0, 0, time.UTC)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The shape of `cswap list --json`, with made-up logins: two in one organization, one of them
// active, and a third whose usage read failed.
const cswapJSON = `{"schemaVersion":1,"activeAccountNumber":2,"accounts":[
 {"number":2,"email":"Ada@Example.com","active":true,"isOrganization":true,"organizationName":"Org","organizationUuid":"org-1",
  "usageStatus":"ok","usageAgeSeconds":12.5,"usageFetchedAt":"2026-11-08T19:29:00Z",
  "usage":{"sevenDay":{"pct":17.0,"resetsAt":"2026-11-13T11:00:00.701749+00:00","expectedPct":33.6},
           "fiveHour":{"pct":42.5,"resetsAt":"2026-11-08T21:00:00.12+00:00"},
           "scoped":[{"name":"Fable","pct":3.0,"resetsAt":"2026-11-13T11:00:00+00:00"}]}},
 {"number":1,"email":"grace@example.com","active":false,"organizationUuid":"org-1",
  "usageStatus":"ok","usageFetchedAt":"2026-11-08T19:20:00Z",
  "usage":{"sevenDay":{"pct":0.0,"resetsAt":"2026-11-14T11:59:59.560424+00:00"},"fiveHour":null,"scoped":[]}},
 {"number":3,"email":"linus@example.com","active":false,"usageStatus":"expired","usageFetchedAt":null,"usage":null}
]}`

func TestParseClaudeListsEveryLogin(t *testing.T) {
	s := parseClaude([]byte(cswapJSON), now)
	if s.Provider != Claude || s.Error != "" {
		t.Fatalf("source = %+v", s)
	}
	want := []Account{
		{ID: "grace@example.com", Label: "grace@example.com", Slot: 1, ObservedAt: at("2026-11-08T19:20:00Z"), Windows: []Window{
			{ID: "7d", Seconds: Week, UsedPct: 0, ResetsAt: at("2026-11-14T12:00:00Z")},
		}},
		{ID: "ada@example.com", Label: "ada@example.com", Slot: 2, Active: true, ObservedAt: at("2026-11-08T19:29:00Z"), Windows: []Window{
			{ID: "7d", Seconds: Week, UsedPct: 17, ResetsAt: at("2026-11-13T11:00:00Z")},
			{ID: "5h", Seconds: FiveHours, UsedPct: 42.5, ResetsAt: at("2026-11-08T21:00:00Z")},
			{ID: "fable/7d", Seconds: Week, UsedPct: 3, ResetsAt: at("2026-11-13T11:00:00Z")},
		}},
		{ID: "linus@example.com", Label: "linus@example.com", Slot: 3, ObservedAt: now, Error: "cswap usage status: expired"},
	}
	if !reflect.DeepEqual(s.Accounts, want) {
		t.Errorf("accounts =\n%+v\nwant\n%+v", s.Accounts, want)
	}
}

func TestParseClaudeKeepsTwoOrganizationsOfOneEmailApart(t *testing.T) {
	s := parseClaude([]byte(`{"accounts":[
		{"number":1,"email":"ada@example.com","organizationUuid":"org-1","usageStatus":"ok","usage":{"sevenDay":{"pct":1,"resetsAt":"2026-11-13T11:00:00Z"}}},
		{"number":2,"email":"ada@example.com","organizationUuid":"org-2","usageStatus":"ok","usage":{"sevenDay":{"pct":2,"resetsAt":"2026-11-13T11:00:00Z"}}}]}`), now)
	if len(s.Accounts) != 2 || s.Accounts[0].ID == s.Accounts[1].ID || !strings.HasPrefix(s.Accounts[1].ID, "ada@example.com#") ||
		strings.Contains(s.Accounts[1].ID, "org-2") {
		t.Errorf("accounts = %+v", s.Accounts)
	}
}

func TestParseClaudeRejectsGarbage(t *testing.T) {
	for _, raw := range []string{`not json`, `{"accounts":[]}`} {
		if s := parseClaude([]byte(raw), now); s.Error == "" || len(s.Accounts) != 0 {
			t.Errorf("%s: %+v", raw, s)
		}
	}
}

const codexJSON = `{"user_id":"u","account_id":"acct-1","email":"Ada@example.com","plan_type":"pro",
 "rate_limit":{"allowed":true,"primary_window":{"used_percent":24,"limit_window_seconds":604800,"reset_at":1794379726},
               "secondary_window":{"used_percent":7.5,"limit_window_seconds":18000,"reset_at":1794175246}}}`

func TestParseCodexMeter(t *testing.T) {
	a, err := parseCodexMeter([]byte(codexJSON), now)
	if err != nil {
		t.Fatal(err)
	}
	want := Account{ID: "ada@example.com", Label: "ada@example.com", Plan: "pro", Active: true, ObservedAt: now, Windows: []Window{
		{ID: "7d", Seconds: Week, UsedPct: 24, ResetsAt: time.Unix(1794379726, 0).UTC().Round(time.Minute)},
		{ID: "5h", Seconds: FiveHours, UsedPct: 7.5, ResetsAt: time.Unix(1794175246, 0).UTC().Round(time.Minute)},
	}}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("account =\n%+v\nwant\n%+v", a, want)
	}
	if _, err := parseCodexMeter([]byte(`{"email":"a@example.com","rate_limit":{}}`), now); err == nil {
		t.Error("a meter with no windows parsed")
	}
}

// fakeJWT is an unsigned token with claims, the shape of Codex's id_token.
func fakeJWT(claims string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(claims) + ".sig"
}

func writeCodexHome(t *testing.T) string {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(filepath.Join(dir, "sessions/2026/11/08"), 0o700); err != nil {
		t.Fatal(err)
	}
	idToken := fakeJWT(`{"email":"ada@example.com","https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`)
	auth := `{"tokens":{"access_token":"tok","account_id":"acct-1","id_token":"` + idToken + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	session := `{"timestamp":"2026-11-08T17:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":20,"window_minutes":10080,"resets_at":1794379726},"secondary":null}}}
{"timestamp":"2026-11-08T17:00:37.749Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":23.0,"window_minutes":10080,"resets_at":1794379726},"secondary":null,"plan_type":"pro"}}}
{"timestamp":"2026-11-08T17:01:00Z","type":"response_item","payload":{"type":"message"}}
`
	if err := os.WriteFile(filepath.Join(dir, "sessions/2026/11/08/rollout-1.jsonl"), []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestReadCodexUsesTheLiveMeterWithTheLoginsToken(t *testing.T) {
	home := writeCodexHome(t)
	var gotAuth, gotAcct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAcct = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
		_, _ = w.Write([]byte(codexJSON))
	}))
	defer srv.Close()
	r := &Reader{Home: home, CodexURL: srv.URL, Now: func() time.Time { return now }}
	s := r.readCodex(context.Background())
	if gotAuth != "Bearer tok" || gotAcct != "acct-1" {
		t.Errorf("headers: %q %q", gotAuth, gotAcct)
	}
	if s.Error != "" || len(s.Accounts) != 1 || s.Accounts[0].Windows[0].UsedPct != 24 || s.Accounts[0].Note != "" {
		t.Errorf("source = %+v", s)
	}
}

func TestReadCodexFallsBackToTheNewestSessionReading(t *testing.T) {
	home := writeCodexHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	r := &Reader{Home: home, CodexURL: srv.URL, Now: func() time.Time { return now }}
	s := r.readCodex(context.Background())
	if s.Error != "" || len(s.Accounts) != 1 {
		t.Fatalf("source = %+v", s)
	}
	a := s.Accounts[0]
	want := Account{ID: "ada@example.com", Label: "ada@example.com", Plan: "pro", Active: true, ObservedAt: at("2026-11-08T17:00:37.749Z"),
		Note: "live meter unavailable (401 Unauthorized); last local session reading", Windows: []Window{
			{ID: "7d", Seconds: Week, UsedPct: 23, ResetsAt: time.Unix(1794379726, 0).UTC().Round(time.Minute)},
		}}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("account =\n%+v\nwant\n%+v", a, want)
	}
}

func TestReadCodexWithoutALoginIsMissing(t *testing.T) {
	r := &Reader{Home: t.TempDir(), Now: func() time.Time { return now }}
	if s := r.readCodex(context.Background()); !s.Missing {
		t.Errorf("source = %+v", s)
	}
}

func TestParseMuse(t *testing.T) {
	s := parseMuse([]byte(`{"allowance":{"asOfMs":1794166595360,"source":"mint","tier":"Muse Code Power Usage",
		"weekly":{"resetsAtMs":1794182400000,"usedPercent":25},"window":{"resetsAtMs":1794175246000,"usedPercent":0}},
		"hint":null,"reason":null,"schemaVersion":1}`), now)
	want := Source{Provider: Muse, Accounts: []Account{{ID: "default", Label: "Muse", Plan: "Muse Code Power Usage", Active: true,
		ObservedAt: time.UnixMilli(1794166595360).UTC(), Windows: []Window{
			{ID: "7d", Seconds: Week, UsedPct: 25, ResetsAt: time.UnixMilli(1794182400000).UTC().Round(time.Minute)},
			{ID: "5h", Seconds: FiveHours, UsedPct: 0, ResetsAt: time.UnixMilli(1794175246000).UTC().Round(time.Minute)},
		}}}}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("source =\n%+v\nwant\n%+v", s, want)
	}
}

func TestParseMuseBeforeFirstUseInAWindowIsNoUsageNotAGuess(t *testing.T) {
	s := parseMuse([]byte(`{"allowance":null,"reason":"server_sent_no_usage","schemaVersion":1}`), now)
	if s.Error != "" || len(s.Accounts) != 1 {
		t.Fatalf("source = %+v", s)
	}
	a := s.Accounts[0]
	if len(a.Windows) != 0 || a.Note == "" || a.Error != "" {
		t.Errorf("account = %+v", a)
	}
	if s := parseMuse([]byte(`{"allowance":null,"reason":"not_logged_in"}`), now); s.Error == "" {
		t.Errorf("logged out: %+v", s)
	}
}

func TestParseAntigravityKeepsEachPool(t *testing.T) {
	s := parseAntigravity([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[
		{"name":"Gemini Models","buckets":[
			{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.75,"reset_time":"2026-11-14T00:00:53Z"},
			{"id":"gemini-5h","window":"5h","remaining_fraction":0.98,"reset_time":"2026-11-08T21:09:58Z"}]},
		{"name":"Claude and GPT models","buckets":[
			{"id":"x-weekly","window":"weekly","remaining_fraction":1,"reset_time":"2026-11-15T19:36:37Z"},
			{"id":"x-5h","window":"5h","remaining_fraction":null,"reset_time":"2026-11-09T00:36:37Z"}]}]}}}`), now)
	if s.Error != "" || len(s.Accounts) != 1 {
		t.Fatalf("source = %+v", s)
	}
	got := s.Accounts[0]
	want := Account{ID: "default", Label: "Antigravity", Active: true, ObservedAt: now, Windows: []Window{
		{ID: "gemini/7d", Seconds: Week, UsedPct: 25, ResetsAt: at("2026-11-14T00:01:00Z")},
		{ID: "gemini/5h", Seconds: FiveHours, UsedPct: 2, ResetsAt: at("2026-11-08T21:10:00Z")},
		{ID: "claude-gpt/7d", Seconds: Week, UsedPct: 0, ResetsAt: at("2026-11-15T19:37:00Z")},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("account =\n%+v\nwant\n%+v", got, want)
	}
	if s := parseAntigravity([]byte(`{"status":"ERROR"}`), now); s.Error == "" {
		t.Errorf("failed status: %+v", s)
	}
}

func TestReadRunsEveryProviderAndMarksMissingTools(t *testing.T) {
	r := &Reader{Home: t.TempDir(), Now: func() time.Time { return now },
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cswap":
				if strings.Join(args, " ") != "list --json" {
					t.Errorf("cswap %v", args)
				}
				return []byte(cswapJSON), nil
			case "agy":
				return nil, errors.New("exit status 1: not signed in")
			}
			return nil, ErrMissing
		}}
	rep := r.Read(context.Background())
	if !rep.CollectedAt.Equal(now) || len(rep.Sources) != 4 {
		t.Fatalf("report = %+v", rep)
	}
	by := map[string]Source{}
	for _, s := range rep.Sources {
		by[s.Provider] = s
	}
	if len(by[Claude].Accounts) != 3 || by[Claude].Error != "" {
		t.Errorf("claude: %+v", by[Claude])
	}
	if !by[Muse].Missing || !by[Codex].Missing {
		t.Errorf("muse, codex: %+v %+v", by[Muse], by[Codex])
	}
	if by[Antigravity].Missing || !strings.Contains(by[Antigravity].Error, "not signed in") {
		t.Errorf("antigravity: %+v", by[Antigravity])
	}
}

func TestRunFindsToolsInLocalBin(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "usage-tool-test"), []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent")
	out, err := (&Reader{Home: home}).run(context.Background(), "usage-tool-test", "a", "b")
	if err != nil || string(out) != "a b\n" {
		t.Errorf("run = %q, %v", out, err)
	}
	if _, err := (&Reader{Home: home}).run(context.Background(), "no-such-usage-tool"); !errors.Is(err, ErrMissing) {
		t.Errorf("missing tool: %v", err)
	}
}
