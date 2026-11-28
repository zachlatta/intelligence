package web

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// testServer is a server on a test database, with that database's account signed in: get and
// call send its session cookie, and openStream streams with each host's key. anon(h) is the same
// server with no one signed in.
type testServer struct {
	local *app
	http.Handler
	d dbtest.DB
	*fixture
}

// fixture is one test database's account, shared by every server instance on it: "someone",
// signed in, with a key for each host a test streams as.
type fixture struct {
	store  *accounts.Store
	acct   accounts.Account
	cookie *http.Cookie

	mu   sync.Mutex
	keys map[string]string
}

var fixtures sync.Map // database name -> *fixture

const testPassword = "a test password"

// testPasswordHash caches one real argon2id hash of testPassword so parallel tests do not
// each allocate 19 MiB (plus race-detector shadow memory) just to create the default "someone"
// fixture account. createFixtureAccount mirrors accounts.Store.Create's insert with that hash.
var (
	testPasswordHashOnce sync.Once
	testPasswordHash     string
	testPasswordHashErr  error
)

func fixtureFor(t *testing.T, d dbtest.DB) *fixture {
	t.Helper()
	if f, ok := fixtures.Load(d.Name); ok {
		return f.(*fixture)
	}
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	f := &fixture{store: accounts.New(d.Owner, roles), keys: map[string]string{}}
	ctx := context.Background()
	var created accounts.Account
	createdInOnce := false
	testPasswordHashOnce.Do(func() {
		created, testPasswordHashErr = f.store.Create(ctx, "someone", testPassword)
		if testPasswordHashErr != nil {
			return
		}
		createdInOnce = true
		testPasswordHashErr = d.Owner.QueryRow(ctx, "SELECT password_hash FROM accounts WHERE id = $1", created.ID).Scan(&testPasswordHash)
	})
	if testPasswordHashErr != nil {
		t.Fatal(testPasswordHashErr)
	}
	if createdInOnce {
		f.acct = created
	} else {
		f.acct = createFixtureAccount(t, d.Owner, roles, "someone", testPasswordHash)
	}
	f.cookie = sessionFor(t, f.store, f.acct)
	fixtures.Store(d.Name, f)
	t.Cleanup(func() { fixtures.Delete(d.Name) })
	return f
}

func createFixtureAccount(t *testing.T, p *pgxpool.Pool, roles *db.AccountRoles, username, hash string) accounts.Account {
	t.Helper()
	ctx := context.Background()
	a := accounts.Account{Username: username}
	err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		var err error
		if err := tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('accounts', 'id'))").Scan(&a.ID); err != nil {
			return err
		}
		if a.Role, err = roles.RoleName(a.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, username, password_hash, role_name, created_at, password_changed_at)
			VALUES ($1, $2, $3, $4, now(), now())`, a.ID, a.Username, hash, a.Role); err != nil {
			return err
		}
		return roles.Ensure(ctx, tx, a.Role)
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sessionFor(t *testing.T, s *accounts.Store, a accounts.Account) *http.Cookie {
	t.Helper()
	tok, _, err := s.StartSession(context.Background(), a, accounts.SessionMetadata{CreatedFromPath: "/login"})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

// joinHost adds host to account a through the enrollment flow, and returns its key.
func joinHost(t *testing.T, s *accounts.Store, a accounts.Account, host string) string {
	t.Helper()
	ctx := context.Background()
	e, err := s.Enroll(ctx, host)
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

// keyFor is host's key in the fixture's account, joining it the first time.
func (f *fixture) keyFor(t *testing.T, host string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[host]; ok {
		return k
	}
	f.keys[host] = joinHost(t, f.store, f.acct, host)
	return f.keys[host]
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

func TestFixtureForReusesPasswordHash(t *testing.T) {
	t.Parallel()
	d1, d2 := dbtest.New(t), dbtest.New(t)
	f1, f2 := fixtureFor(t, d1), fixtureFor(t, d2)

	var h1, h2 string
	if err := d1.Owner.QueryRow(t.Context(), "SELECT password_hash FROM accounts WHERE id = $1", f1.acct.ID).Scan(&h1); err != nil {
		t.Fatal(err)
	}
	if err := d2.Owner.QueryRow(t.Context(), "SELECT password_hash FROM accounts WHERE id = $1", f2.acct.ID).Scan(&h2); err != nil {
		t.Fatal(err)
	}
	if h1 == "" || h1 != h2 {
		t.Fatalf("fixtureFor computed distinct password hashes (%q vs %q); want one shared argon2id hash", h1, h2)
	}
	if _, err := f2.store.Authenticate(t.Context(), "someone", testPassword); err != nil {
		t.Fatalf("reused fixture hash failed Authenticate: %v", err)
	}
}

func keyOf(t *testing.T, h http.Handler, host string) string {
	t.Helper()
	ts, ok := h.(*testServer)
	if !ok {
		t.Fatal("not a test server")
	}
	return ts.keyFor(t, host)
}
