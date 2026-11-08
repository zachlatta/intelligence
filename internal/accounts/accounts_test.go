package accounts_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

var ctx = context.Background()

func store(t *testing.T) (*accounts.Store, dbtest.DB) {
	t.Helper()
	d := dbtest.New(t)
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	return accounts.New(d.Owner, roles), d
}

const goodPassword = "correct horse battery"

func TestCreateAndSignIn(t *testing.T) {
	s, d := store(t)
	a, err := s.Create(ctx, " Ann ", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if a.Username != "ann" || a.ID == 0 || a.Role == "" {
		t.Fatalf("created %+v", a)
	}
	if _, err := s.Create(ctx, "ann", goodPassword); !errors.Is(err, accounts.ErrTaken) {
		t.Errorf("same name again: %v", err)
	}
	for name, pw := range map[string]string{"x y": goodPassword, "": goodPassword, strings.Repeat("a", 33): goodPassword, "bob": "short"} {
		if _, err := s.Create(ctx, name, pw); err == nil || errors.Is(err, accounts.ErrTaken) {
			t.Errorf("create %q/%q: %v, want refused", name, pw, err)
		}
	}
	got, err := s.Authenticate(ctx, "ANN", goodPassword)
	if err != nil || got.ID != a.ID {
		t.Fatalf("sign in: %+v %v", got, err)
	}
	for name, pw := range map[string]string{"ann": "wrong password!", "nobody": goodPassword} {
		if _, err := s.Authenticate(ctx, name, pw); !errors.Is(err, accounts.ErrBadCredentials) {
			t.Errorf("%s/%s: %v", name, pw, err)
		}
	}
	var hash string
	if err := d.Owner.QueryRow(ctx, "SELECT password_hash FROM accounts").Scan(&hash); err != nil || !strings.HasPrefix(hash, "$argon2id$") ||
		strings.Contains(hash, goodPassword) {
		t.Errorf("stored %q (%v)", hash, err)
	}
}

func TestSessions(t *testing.T) {
	s, d := store(t)
	a, _ := s.Create(ctx, "ann", goodPassword)
	tok, exp, err := s.StartSession(ctx, a)
	if err != nil || len(tok) < 40 || exp.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("session %q %v %v", tok, exp, err)
	}
	if got, err := s.Session(ctx, tok); err != nil || got.ID != a.ID || got.Username != "ann" {
		t.Fatalf("session reads %+v %v", got, err)
	}
	var stored string
	d.Owner.QueryRow(ctx, "SELECT token_hash FROM sessions").Scan(&stored)
	if stored == tok || stored == "" {
		t.Errorf("the token itself is stored")
	}
	if _, err := s.Session(ctx, "nope"); !errors.Is(err, accounts.ErrNoSession) {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := d.Owner.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, tok); !errors.Is(err, accounts.ErrNoSession) {
		t.Errorf("expired: %v", err)
	}
	tok2, _, _ := s.StartSession(ctx, a)
	if err := s.EndSession(ctx, tok2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, tok2); !errors.Is(err, accounts.ErrNoSession) {
		t.Errorf("after sign out: %v", err)
	}
}

func TestChangingThePasswordEndsOtherSessions(t *testing.T) {
	s, _ := store(t)
	a, _ := s.Create(ctx, "ann", goodPassword)
	other, _, _ := s.StartSession(ctx, a)
	if err := s.ChangePassword(ctx, a.ID, "wrong password!", "a brand new password"); !errors.Is(err, accounts.ErrBadCredentials) {
		t.Errorf("wrong current: %v", err)
	}
	if err := s.ChangePassword(ctx, a.ID, goodPassword, "short"); err == nil {
		t.Errorf("a short new password was taken")
	}
	if err := s.ChangePassword(ctx, a.ID, goodPassword, "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, other); !errors.Is(err, accounts.ErrNoSession) {
		t.Errorf("an old session survived: %v", err)
	}
	if _, err := s.Authenticate(ctx, "ann", "a brand new password"); err != nil {
		t.Errorf("new password: %v", err)
	}
}

func TestAHostJoinsAnAccountWithACode(t *testing.T) {
	s, d := store(t)
	ann, _ := s.Create(ctx, "ann", goodPassword)
	bob, _ := s.Create(ctx, "bob", goodPassword)
	e, err := s.Enroll(ctx, "host-1")
	if err != nil || e.ID == "" || len(e.Secret) < 40 || len(e.Code) != 9 || e.Code[4] != '-' {
		t.Fatalf("enroll: %+v %v", e, err)
	}
	if _, err := s.Enroll(ctx, "Bad Name"); err == nil {
		t.Errorf("a bad host name enrolled")
	}
	if _, _, err := s.Collect(ctx, e.ID, e.Secret); !errors.Is(err, accounts.ErrNotClaimed) {
		t.Errorf("collect before the claim: %v", err)
	}
	if _, err := s.Claim(ctx, ann, "ZZZZ-ZZZZ"); !errors.Is(err, accounts.ErrNoSuchCode) {
		t.Errorf("a wrong code: %v", err)
	}
	// codes are forgiving: any case, with or without the dash, spaces around
	host, err := s.Claim(ctx, ann, " "+strings.ToLower(strings.ReplaceAll(e.Code, "-", ""))+" ")
	if err != nil || host != "host-1" {
		t.Fatalf("claim: %q %v", host, err)
	}
	if _, _, err := s.Collect(ctx, e.ID, "not-the-secret"); !errors.Is(err, accounts.ErrNoSuchEnrollment) {
		t.Errorf("collect with a wrong secret: %v", err)
	}
	key, gotHost, err := s.Collect(ctx, e.ID, e.Secret)
	if err != nil || gotHost != "host-1" || len(key) < 40 {
		t.Fatalf("collect: %q %q %v", key, gotHost, err)
	}
	if _, _, err := s.Collect(ctx, e.ID, e.Secret); !errors.Is(err, accounts.ErrNoSuchEnrollment) {
		t.Errorf("a key is collected once: %v", err)
	}
	if ok, err := s.HostKey(ctx, "host-1", key); !ok || err != nil {
		t.Errorf("the key: %v %v", ok, err)
	}
	for host, k := range map[string]string{"host-1": "wrong", "host-2": key} {
		if ok, _ := s.HostKey(ctx, host, k); ok {
			t.Errorf("%s with key %q accepted", host, k)
		}
	}
	if owns, _ := s.Owns(ctx, ann.ID, "host-1"); !owns {
		t.Errorf("ann does not own host-1")
	}
	if owns, _ := s.Owns(ctx, bob.ID, "host-1"); owns {
		t.Errorf("bob owns host-1")
	}
	// another account cannot take a host name that is ann's
	e2, _ := s.Enroll(ctx, "host-1")
	if _, err := s.Claim(ctx, bob, e2.Code); !errors.Is(err, accounts.ErrHostTaken) {
		t.Errorf("bob claims ann's host: %v", err)
	}
	// ann can: the host gets a new key, and the old one stops working
	e3, _ := s.Enroll(ctx, "host-1")
	if _, err := s.Claim(ctx, ann, e3.Code); err != nil {
		t.Fatal(err)
	}
	key2, _, err := s.Collect(ctx, e3.ID, e3.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HostKey(ctx, "host-1", key); ok {
		t.Errorf("the replaced key still works")
	}
	if ok, _ := s.HostKey(ctx, "host-1", key2); !ok {
		t.Errorf("the new key does not work")
	}
	var stored string
	d.Owner.QueryRow(ctx, "SELECT key_hash FROM host_keys").Scan(&stored)
	if stored == key2 {
		t.Errorf("the key itself is stored")
	}
	if _, err := d.Owner.Exec(ctx, "INSERT INTO hosts (name, last_seen) VALUES ('host-1', now()); INSERT INTO host_probes (host, running, received_at) VALUES ('host-1', true, now())"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveHost(ctx, ann.ID, "host-1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HostKey(ctx, "host-1", key2); ok {
		t.Errorf("a removed host's key works")
	}
	var left int
	d.Owner.QueryRow(ctx, "SELECT (SELECT count(*) FROM hosts) + (SELECT count(*) FROM host_probes)").Scan(&left)
	if left != 0 {
		t.Errorf("a removed host's reports are kept: %d rows", left)
	}
	if err := s.RemoveHost(ctx, bob.ID, "host-1"); !errors.Is(err, accounts.ErrNoSuchHost) {
		t.Errorf("removing a host that is not yours: %v", err)
	}
}

func TestAnExpiredCodeCannotBeClaimed(t *testing.T) {
	s, d := store(t)
	ann, _ := s.Create(ctx, "ann", goodPassword)
	e, _ := s.Enroll(ctx, "host-1")
	d.Owner.Exec(ctx, "UPDATE host_enrollments SET expires_at = now() - interval '1 second'")
	if _, err := s.Claim(ctx, ann, e.Code); !errors.Is(err, accounts.ErrNoSuchCode) {
		t.Errorf("claim an expired code: %v", err)
	}
	if _, _, err := s.Collect(ctx, e.ID, e.Secret); !errors.Is(err, accounts.ErrNoSuchEnrollment) {
		t.Errorf("collect an expired enrollment: %v", err)
	}
}

func TestEachAccountReadsThroughItsOwnRole(t *testing.T) {
	s, _ := store(t)
	ann, _ := s.Create(ctx, "ann", goodPassword)
	s.Create(ctx, "bob", goodPassword)
	p, err := s.Reader(ctx, ann)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, _ := p.Query(ctx, "SELECT username FROM accounts")
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if len(names) != 1 || names[0] != "ann" {
		t.Errorf("ann reads accounts %v", names)
	}
}
