package web

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// testServer is a server on a test database, with that database's account signed in: get and
// call send its session cookie, and openStream streams with each box's key. anon(h) is the same
// server with no one signed in.
type testServer struct {
	http.Handler
	d dbtest.DB
	*fixture
}

// fixture is one test database's account, shared by every server instance on it: "someone",
// signed in, with a key for each box a test streams as.
type fixture struct {
	store  *accounts.Store
	acct   accounts.Account
	cookie *http.Cookie

	mu   sync.Mutex
	keys map[string]string
}

var fixtures sync.Map // database name -> *fixture

const testPassword = "a test password"

func fixtureFor(t *testing.T, d dbtest.DB) *fixture {
	t.Helper()
	if f, ok := fixtures.Load(d.Name); ok {
		return f.(*fixture)
	}
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	f := &fixture{store: accounts.New(d.Owner, roles), keys: map[string]string{}}
	var err error
	if f.acct, err = f.store.Create(context.Background(), "someone", testPassword); err != nil {
		t.Fatal(err)
	}
	f.cookie = sessionFor(t, f.store, f.acct)
	fixtures.Store(d.Name, f)
	t.Cleanup(func() { fixtures.Delete(d.Name) })
	return f
}

func sessionFor(t *testing.T, s *accounts.Store, a accounts.Account) *http.Cookie {
	t.Helper()
	tok, _, err := s.StartSession(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

// joinBox adds box to account a through the enrollment flow, and returns its key.
func joinBox(t *testing.T, s *accounts.Store, a accounts.Account, box string) string {
	t.Helper()
	ctx := context.Background()
	e, err := s.Enroll(ctx, box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, a, e.Code); err != nil {
		t.Fatal(err)
	}
	key, _, err := s.Collect(ctx, e.ID, e.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// keyFor is box's key in the fixture's account, joining it the first time.
func (f *fixture) keyFor(t *testing.T, box string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[box]; ok {
		return k
	}
	f.keys[box] = joinBox(t, f.store, f.acct, box)
	return f.keys[box]
}

// newServer is a server instance on d's database through p, at version v, with d's account.
func newServer(t *testing.T, d dbtest.DB, p *pgxpool.Pool, v string, mod ...func(*Config)) *testServer {
	t.Helper()
	cfg := Config{Context: t.Context(), Version: v, DB: p, Readonly: d.Readonly}
	for _, m := range mod {
		m(&cfg)
	}
	return &testServer{Handler: New(cfg), d: d, fixture: fixtureFor(t, d)}
}

// anon is h with no one signed in.
func anon(h http.Handler) http.Handler {
	if ts, ok := h.(*testServer); ok {
		return ts.Handler
	}
	return h
}

// signedIn adds the fixture's session cookie to req when h is a test server and req has none.
func signedIn(h http.Handler, req *http.Request) {
	if ts, ok := h.(*testServer); ok && req.Header.Get("Cookie") == "" {
		req.AddCookie(ts.cookie)
	}
}

// keyOf is box's key on test server h.
func keyOf(t *testing.T, h http.Handler, box string) string {
	t.Helper()
	ts, ok := h.(*testServer)
	if !ok {
		t.Fatal("not a test server")
	}
	return ts.keyFor(t, box)
}
