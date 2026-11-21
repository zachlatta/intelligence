// Package dbtest gives each test its own migrated Postgres database and read-only role, on the
// server in TEST_DATABASE_URL (a superuser URL; `make test` starts a throwaway one).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/db"
)

// DB is one test's database: the owner pool the app writes with, and the read-only pool.
type DB struct {
	Owner    *pgxpool.Pool
	Readonly *pgxpool.Pool
	Name     string // the database
	Role     string // the read-only role

	OwnerURL    string // how Owner connects
	ReadonlyURL string // how Readonly connects
}

var migratedTemplate *databaseTemplate

type databaseTemplate struct {
	once sync.Once
	name string
	err  error
}

// RunWithTemplate migrates one database on first use and clones it for subsequent fixtures.
// Call it from TestMain. Each New still makes its own database and roles.
func RunWithTemplate(m *testing.M) int {
	tmpl := &databaseTemplate{}
	migratedTemplate = tmpl
	code := m.Run()
	if tmpl.name != "" {
		if err := tmpl.drop(); err != nil {
			fmt.Fprintln(os.Stderr, "drop test database template:", err)
			return 1
		}
	}
	return code
}

func (tmpl *databaseTemplate) prepare(ctx context.Context, admin *pgx.Conn, name, dsn string) {
	if _, tmpl.err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); tmpl.err != nil {
		return
	}
	tmpl.name = name
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tmpl.err = err
		return
	}
	defer p.Close()
	tmpl.err = db.Migrate(ctx, p)
}

func (tmpl *databaseTemplate) drop() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{tmpl.name}.Sanitize())
	return err
}

// New makes a fresh migrated database and sets up its read-only role. Both go when the test ends.
func New(t testing.TB) DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is not set: run make test or make test-pkg PKG=./internal/yourpackage")
	}
	ctx := context.Background()
	suffix := randHex(t)
	name, role, pass := "t_"+suffix, "ro_"+suffix, randHex(t)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	create := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if tmpl := migratedTemplate; tmpl != nil {
		tmpl.once.Do(func() {
			templateName := "tmpl_" + suffix
			tmpl.prepare(ctx, admin, templateName, withDB(t, base, templateName, "", ""))
		})
		if tmpl.err != nil {
			t.Fatalf("prepare test database template: %v", tmpl.err)
		}
		create += " TEMPLATE " + pgx.Identifier{tmpl.name}.Sanitize()
	}
	if _, err := admin.Exec(ctx, create); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		// each account's role (db.AccountRoles) is named after the read-only role
		if rows, err := admin.Query(ctx, "SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)", role+"_a"); err == nil {
			accountRoles, _ := pgx.CollectRows(rows, pgx.RowTo[string])
			for _, r := range accountRoles {
				_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{r}.Sanitize())
			}
		}
		_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})

	ownerURL := withDB(t, base, name, "", "")
	owner := pool(t, ownerURL)
	if migratedTemplate == nil {
		if err := db.Migrate(ctx, owner); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	if err := db.EnsureReadonlyRole(ctx, owner, role, pass); err != nil {
		t.Fatalf("read-only role: %v", err)
	}
	roURL := withDB(t, base, name, role, pass)
	return DB{Owner: owner, Readonly: pool(t, roURL), Name: name, Role: role, OwnerURL: ownerURL, ReadonlyURL: roURL}
}

// Pool is another owner pool on d's database, as a second server instance would have.
func Pool(t testing.TB, d DB) *pgxpool.Pool {
	t.Helper()
	return pool(t, d.OwnerURL)
}

// Reconnect gives d a fresh read-only pool, so role settings changed since apply.
func Reconnect(t testing.TB, d DB) DB {
	t.Helper()
	d.Readonly.Close()
	d.Readonly = pool(t, d.ReadonlyURL)
	return d
}

// ReconnectAs gives d a fresh read-only pool that logs in with password, after it changed.
func ReconnectAs(t testing.TB, d DB, role, password string) DB {
	t.Helper()
	u, err := url.Parse(d.ReadonlyURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	d.ReadonlyURL = u.String()
	return Reconnect(t, d)
}

func pool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// withDB is base pointed at database name, as user/pass when user is set.
func withDB(t testing.TB, base, name, user, pass string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func randHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Reader is the read pool of the account that owns host, made (and given the host) if no account
// does: it reads as the SQL API does for that account, only that account's rows.
func Reader(t testing.TB, d DB, host string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	var role string
	err := d.Owner.QueryRow(ctx, "SELECT a.role_name FROM host_keys k JOIN accounts a ON a.id = k.account_id WHERE k.host = $1", host).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		var id int64
		if err := d.Owner.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('accounts', 'id'))").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if role, err = roles.RoleName(id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, `INSERT INTO accounts (id, username, password_hash, role_name) VALUES ($1, $2, '-', $3)`, id, fmt.Sprintf("reader%d", id), role); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, `INSERT INTO host_keys (host, account_id, key_hash, created_at) VALUES ($1, $2, md5(random()::text), now())`, host, id); err != nil {
			t.Fatal(err)
		}
		err = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := roles.Pool(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
