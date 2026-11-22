package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/usage"
)

const expiredLogin = `{"type": "assistant_message", "text": "Failed to authenticate: this login reached its time limit and cannot be renewed. Run ` + "`claude auth login`" + ` to sign in again, or use ` + "`claude setup-token`" + ` and set CLAUDE_CODE_OAUTH_TOKEN for unattended jobs"}`

func entryAt(start int, at time.Time, item string) hostclient.TranscriptEntry {
	e := entry(start, start, item)
	e.At = at
	return e
}

type interruption struct {
	Agent, Reason, Status, Login string
	Seq                          int
	Incident                     *int64
}

func interruptions(t *testing.T, r *Registry) []interruption {
	t.Helper()
	rows, err := r.p.Query(ctx, `SELECT x.agent_id, x.seq, x.reason, x.status, coalesce(i.login, '-'), x.incident_id
		FROM agent_interruptions x LEFT JOIN login_incidents i ON i.id = x.incident_id ORDER BY x.agent_id, x.seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []interruption
	for rows.Next() {
		var x interruption
		if err := rows.Scan(&x.Agent, &x.Seq, &x.Reason, &x.Status, &x.Login, &x.Incident); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	return out
}

type incident struct {
	ID                        int64
	Login, Reason, Source, By string
	Opened, Seen              time.Time
	Resolved                  *time.Time
}

func incidents(t *testing.T, r *Registry) []incident {
	t.Helper()
	rows, err := r.p.Query(ctx, `SELECT id, login, reason, source, resolved_by, opened_at, last_seen_at, resolved_at
		FROM login_incidents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []incident
	for rows.Next() {
		var x incident
		if err := rows.Scan(&x.ID, &x.Login, &x.Reason, &x.Source, &x.By, &x.Opened, &x.Seen, &x.Resolved); err != nil {
			t.Fatal(err)
		}
		x.Opened, x.Seen = x.Opened.UTC(), x.Seen.UTC()
		out = append(out, x)
	}
	return out
}

// The 2026-11-19 case: the host's active Claude login reached its time limit, and every Claude
// agent's next turn came back as Claude Code's "Failed to authenticate" message.
func TestLoginFailureInATranscriptIsAnInterruptionOfTheHostsLogin(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(40, t1.Add(72*time.Hour)))))
	fail := t1.Add(10 * time.Minute)
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(1, fail.Add(-time.Second), userMsg), entryAt(2, fail, expiredLogin)}}, fail.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// a resend (a reconnect, or the whole timeline again) is the same interruption
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(1, fail.Add(-time.Second), userMsg), entryAt(2, fail, expiredLogin)}}, fail.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	// another agent on the host fails a minute later: same broken login
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(7, fail.Add(time.Minute), expiredLogin)}}, fail.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := interruptions(t, r)
	if len(got) != 2 || got[0].Agent != "a1" || got[1].Agent != "a3" || got[1].Seq != 2 {
		t.Fatalf("interruptions = %+v", got)
	}
	for _, x := range got {
		if x.Reason != "login_expired" || x.Status != "waiting" || x.Login != "ada@example.com" || x.Incident == nil || *x.Incident != *got[0].Incident {
			t.Fatalf("interruption = %+v", x)
		}
	}
	inc := incidents(t, r)
	if len(inc) != 1 || inc[0].Source != "transcript" || !inc[0].Opened.Equal(fail) || !inc[0].Seen.Equal(fail.Add(time.Minute)) || inc[0].Resolved != nil {
		t.Fatalf("incidents = %+v", inc)
	}
	// readable by the host's account through its read-only role, with the banner's view
	var label, kind string
	var n, waiting int
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT kind, label, interrupted, waiting FROM login_alerts`).
		Scan(&kind, &label, &n, &waiting); err != nil {
		t.Fatal(err)
	}
	if kind != "broken" || label != "ada@example.com" || n != 2 || waiting != 2 {
		t.Fatalf("alert = %s %s %d %d", kind, label, n, waiting)
	}
}

// A first sync sends an agent's whole timeline: a failure from long ago is history, not news.
func TestOldLoginFailuresAreNotInterruptions(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), t1)
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(2, t1, expiredLogin)}}, t1.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := interruptions(t, r); len(got) != 0 {
		t.Fatalf("interruptions = %+v", got)
	}
}

func TestAgentsTalkingAboutLoginsAreNotInterruptions(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), t1)
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true, Entries: []hostclient.TranscriptEntry{
		entryAt(1, time.Now(), `{"type": "assistant_message", "text": "All 4 uploads are verified and an anonymous GET returns 401."}`),
		entryAt(2, time.Now(), `{"type": "user_message", "text": "Not logged in · Please run /login"}`)}})
	if got := interruptions(t, r); len(got) != 0 {
		t.Fatalf("interruptions = %+v", got)
	}
}

func TestAFreshWorkingReadingOfTheLoginClosesItsIncident(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), t1)
	reset := t1.Add(72 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(40, reset))))
	fail := t1.Add(10 * time.Minute)
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(2, fail, expiredLogin)}}, fail); err != nil {
		t.Fatal(err)
	}
	// cswap serves its cached value from before the failure: that proves nothing
	cached := ada(false, week(40, reset))
	cached.ObservedAt = fail.Add(-time.Minute)
	recordUsage(t, r, "host-1", claudeRead(fail.Add(5*time.Minute), cached, grace(true, week(1, reset))))
	// and the meter says the sign-in is gone
	broken := ada(false)
	broken.Error = "cswap usage status: relogin_required"
	recordUsage(t, r, "host-1", claudeRead(fail.Add(10*time.Minute), broken, grace(true, week(1, reset))))
	if inc := incidents(t, r); len(inc) != 1 || inc[0].Resolved != nil {
		t.Fatalf("incidents = %+v", inc)
	}
	// someone signs it in again: the next reading of it works
	recordUsage(t, r, "host-1", claudeRead(fail.Add(15*time.Minute), ada(false, week(41, reset)), grace(true, week(1, reset))))
	inc := incidents(t, r)
	if len(inc) != 1 || inc[0].Resolved == nil || inc[0].By != "reading" || !inc[0].Resolved.Equal(fail.Add(15*time.Minute)) {
		t.Fatalf("incidents = %+v", inc)
	}
}

// The meter alone can say a login is broken, before any agent trips over it.
func TestTheMeterOpensAnIncidentForALoginWithNoSignIn(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	reset := t1.Add(72 * time.Hour)
	gone := grace(false)
	gone.Error = "cswap usage status: no_credentials"
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true, week(1, reset)), gone))
	recordUsage(t, r, "host-1", claudeRead(t1.Add(5*time.Minute), ada(true, week(1, reset)), gone))
	// a deferred refresh is cswap's business, not a broken login
	later := ada(true)
	later.Error = "cswap usage status: token_expired"
	recordUsage(t, r, "host-1", claudeRead(t1.Add(6*time.Minute), later, gone))
	inc := incidents(t, r)
	if len(inc) != 1 || inc[0].Login != "grace@example.com" || inc[0].Reason != "logged_out" || inc[0].Source != "meter" ||
		!inc[0].Opened.Equal(t1) || !inc[0].Seen.Equal(t1.Add(6*time.Minute)) {
		t.Fatalf("incidents = %+v", inc)
	}
	var fix, command string
	var first bool
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT fix, command, first_step FROM login_alerts WHERE login = 'grace@example.com'`).
		Scan(&fix, &command, &first); err != nil || fix == "" {
		t.Fatalf("fix %q %v", fix, err)
	}
	safeRelogin(t, command, "grace@example.com")
	if first {
		t.Fatal("a cswap login's command is the whole fix, not a first step")
	}
}

func TestLoginExpiryIsStoredAndWarnedAboutAhead(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	now := time.Now().UTC().Truncate(time.Second)
	soon, later := ada(true, week(1, now.Add(72*time.Hour))), grace(false, week(1, now.Add(72*time.Hour)))
	soon.LoginExpiresAt, later.LoginExpiresAt = now.Add(3*24*time.Hour), now.Add(20*24*time.Hour)
	recordUsage(t, r, "host-1", claudeRead(now, soon, later))
	var at time.Time
	if err := r.p.QueryRow(ctx, `SELECT login_expires_at FROM usage_sources WHERE account = 'grace@example.com'`).Scan(&at); err != nil || !at.Equal(later.LoginExpiresAt) {
		t.Fatal(at, err)
	}
	rows, err := dbtest.Reader(t, d, "host-1").Query(ctx, `SELECT kind, login, fix, command FROM login_alerts ORDER BY login`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, login, fix, command string
		if err := rows.Scan(&kind, &login, &fix, &command); err != nil {
			t.Fatal(err)
		}
		safeRelogin(t, command, login)
		got = append(got, kind+" "+login)
		if fix == "" {
			t.Errorf("%s has no fix", login)
		}
	}
	if len(got) != 1 || got[0] != "expiring ada@example.com" {
		t.Fatalf("alerts = %v", got)
	}
}

func TestMarkingALoginFixedClosesItsIncident(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	gone := grace(false)
	gone.Error = "cswap usage status: no_credentials"
	recordUsage(t, r, "host-1", claudeRead(t1, ada(true), gone))
	id := incidents(t, r)[0].ID
	dbtest.Reader(t, d, "host-1") // makes the host's account
	var owner int64
	if err := d.Owner.QueryRow(ctx, "SELECT account_id FROM host_keys WHERE host = 'host-1'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveLoginIncident(ctx, owner+1000, id, "done", t1.Add(time.Hour)); err != ErrNotYours {
		t.Fatalf("another account: %v", err)
	}
	if err := r.ResolveLoginIncident(ctx, owner, id, "signed in again", t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveLoginIncident(ctx, owner, id, "again", t1.Add(2*time.Hour)); err != nil {
		t.Fatal("resolving twice is fine: ", err)
	}
	inc := incidents(t, r)
	if inc[0].Resolved == nil || inc[0].By != "manual" || !inc[0].Resolved.Equal(t1.Add(time.Hour)) {
		t.Fatalf("incidents = %+v", inc)
	}
	// still broken by the meter's next read: a new incident, not the closed one again
	recordUsage(t, r, "host-1", claudeRead(t1.Add(2*time.Hour), ada(true), gone))
	if inc := incidents(t, r); len(inc) != 2 || inc[1].Resolved != nil {
		t.Fatalf("incidents = %+v", inc)
	}
}

// safeRelogin checks a copyable fix for a cswap-managed Claude login: pasting it must never sign in
// over the live login the host's agents use. It signs in inside a throwaway CLAUDE_CONFIG_DIR, as
// Zach did on 2026-11-19, and loads that login into cswap.
func safeRelogin(t *testing.T, command, login string) {
	t.Helper()
	for _, want := range []string{
		`T=$(mktemp -d)`,
		`CLAUDE_CONFIG_DIR="$T/cfg" claude auth login --claudeai --email '` + login + `'`,
		`CLAUDE_CONFIG_DIR="$T/cfg" XDG_DATA_HOME="$T/data" cswap add`,
		`CLAUDE_CONFIG_DIR="$T/cfg" XDG_DATA_HOME="$T/data" cswap export "$T/export.json"`,
		`cswap import "$T/export.json" --force`,
	} {
		if !strings.Contains(command, want) {
			t.Errorf("command %q lacks %q", command, want)
		}
	}
	if strings.HasPrefix(command, "claude ") || strings.Contains(command, "&& claude auth login") {
		t.Errorf("command %q signs in over the live login", command)
	}
}

// Where the host's login is unknown (no cswap meter), the command is only the first step.
func TestAnUnknownLoginsCommandIsAFirstStep(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	now := time.Now()
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(2, now, expiredLogin)}}, now); err != nil {
		t.Fatal(err)
	}
	var fix, command string
	var first bool
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT fix, command, first_step FROM login_alerts`).Scan(&fix, &command, &first); err != nil {
		t.Fatal(err)
	}
	if !first || command != "claude auth login --claudeai" || !strings.HasPrefix(fix, "First step:") || !strings.Contains(fix, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("fix %q command %q first %v", fix, command, first)
	}
}

func TestClaudeCLILoginFixDoesNotAssumeCswapAndRequiresNewSignIn(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	now := time.Now().UTC().Truncate(time.Second)
	exp1 := now.Add(2 * 24 * time.Hour)
	cliAcct := usage.Account{
		ID:             "ada@example.com",
		Label:          "ada@example.com",
		Active:         true,
		AuthSource:     usage.AuthClaudeCLI,
		ObservedAt:     now,
		LoginExpiresAt: exp1,
	}
	recordUsage(t, r, "host-1", claudeRead(now, cliAcct))
	// Expiring alert on a claude_cli host should give plain claude auth login --email, not cswap.
	var fix, command string
	var first bool
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT fix, command, first_step FROM login_alerts WHERE login = 'ada@example.com'`).
		Scan(&fix, &command, &first); err != nil {
		t.Fatal(err)
	}
	if first || strings.Contains(command, "cswap") || command != "claude auth login --claudeai --email 'ada@example.com'" {
		t.Fatalf("claude_cli alert: first=%v command=%q fix=%q", first, command, fix)
	}
	// Transcript 401 opens an incident; a routine claude auth status with the same loginExpiresAt must NOT close it.
	fail := now.Add(time.Minute)
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "e", Replace: true,
		Entries: []hostclient.TranscriptEntry{entryAt(2, fail, expiredLogin)}}, fail); err != nil {
		t.Fatal(err)
	}
	cliAcct.ObservedAt = fail.Add(5 * time.Minute)
	recordUsage(t, r, "host-1", claudeRead(fail.Add(5*time.Minute), cliAcct))
	if inc := incidents(t, r); len(inc) != 1 || inc[0].Resolved != nil {
		t.Fatalf("routine claude_cli check falsely closed incident: %+v", inc)
	}
	// A fresh sign-in advances loginExpiresAt and DOES close the incident.
	cliAcct.ObservedAt = fail.Add(10 * time.Minute)
	cliAcct.LoginExpiresAt = now.Add(30 * 24 * time.Hour)
	recordUsage(t, r, "host-1", claudeRead(fail.Add(10*time.Minute), cliAcct))
	if inc := incidents(t, r); len(inc) != 1 || inc[0].Resolved == nil || inc[0].By != "reading" {
		t.Fatalf("fresh claude_cli sign-in did not close incident: %+v", inc)
	}
}
