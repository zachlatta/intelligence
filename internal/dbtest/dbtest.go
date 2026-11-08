// Package dbtest gives each test its own migrated Postgres database and read-only role, on the
// server in TEST_DATABASE_URL (a superuser URL; `make test` starts a throwaway one).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
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

// New makes a fresh database, migrates it and sets up its read-only role. Both go when the test ends.
func New(t testing.TB) DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is not set: run make test, which starts a throwaway Postgres")
	}
	ctx := context.Background()
	suffix := randHex(t)
	name, role, pass := "t_"+suffix, "ro_"+suffix, randHex(t)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
		_ = admin.Close(ctx)
	})

	ownerURL := withDB(t, base, name, "", "")
	owner := pool(t, ownerURL)
	if err := db.Migrate(ctx, owner); err != nil {
		t.Fatalf("migrate: %v", err)
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
