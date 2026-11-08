// Package db holds Intelligence's state in Postgres and keeps it readable by a read-only role.
//
// The contract: every piece of app state is in a table or view in the public schema, and the
// read-only role can SELECT all of it and write none of it (CheckContract).
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLock serializes migrations across app instances starting at once.
const migrateLock = 7356216601

// Migrate applies the migrations not yet recorded in schema_migrations, in name order.
func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLock) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
	}
	return nil
}

// EnsureReadonlyRole creates or updates the login role the SQL API connects as: it can read every
// table and view in the public schema, now and later, and its sessions are read-only by default.
// The pool must connect as a role that can create roles and owns the app's tables.
func EnsureReadonlyRole(ctx context.Context, p *pgxpool.Pool, role, password string) error {
	if role == "" || password == "" {
		return errors.New("read-only role needs a name and a password")
	}
	r := pgx.Identifier{role}.Sanitize()
	var exists bool
	if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return err
	}
	verb := "ALTER"
	if !exists {
		verb = "CREATE"
	}
	var setPassword string
	if err := p.QueryRow(ctx, "SELECT format('"+verb+" ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS PASSWORD %L', $1::text, $2::text)",
		role, password).Scan(&setPassword); err != nil {
		return err
	}
	var dbName string
	if err := p.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		return err
	}
	stmts := []string{
		setPassword,
		"ALTER ROLE " + r + " SET default_transaction_read_only = on",
		"ALTER ROLE " + r + " SET statement_timeout = '10s'",
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{dbName}.Sanitize() + " TO " + r,
		"GRANT USAGE ON SCHEMA public TO " + r,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + r,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO " + r,
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
	}
	for _, s := range stmts {
		if _, err := p.Exec(ctx, s); err != nil {
			return fmt.Errorf("read-only role: %w", err)
		}
	}
	return nil
}

// CheckContract verifies that all app state is readable, and only readable, by the read-only pool:
// no table or view outside the public schema, every relation in it readable by SELECT, and no write
// possible. It returns every failure it finds.
func CheckContract(ctx context.Context, owner, readonly *pgxpool.Pool) error {
	var problems []string
	rows, err := owner.Query(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%' ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	rels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var v [2]string
		return v, r.Scan(&v[0], &v[1])
	})
	if err != nil {
		return err
	}
	if len(rels) == 0 {
		problems = append(problems, "no tables found: the database is not migrated")
	}
	for _, rel := range rels {
		if rel[0] != "public" {
			problems = append(problems, fmt.Sprintf("%s.%s is outside the public schema, where the SQL API cannot see it", rel[0], rel[1]))
			continue
		}
		q := "SELECT * FROM " + pgx.Identifier{rel[0], rel[1]}.Sanitize() + " LIMIT 1"
		r, err := readonly.Query(ctx, q)
		if err == nil {
			for r.Next() {
			}
			err = r.Err()
			r.Close()
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("read-only role cannot read %s: %v", rel[1], err))
		}
	}
	// a write must fail even when attempted: roll back whatever happens
	tx, err := readonly.Begin(ctx)
	if err != nil {
		return err
	}
	_, werr := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ('contract-check')")
	_ = tx.Rollback(ctx)
	if werr == nil {
		problems = append(problems, "read-only role can write: an insert into schema_migrations succeeded")
	}
	var super, readOnlyDefault bool
	if err := readonly.QueryRow(ctx, "SELECT rolsuper, current_setting('default_transaction_read_only') = 'on' FROM pg_roles WHERE rolname = current_user").
		Scan(&super, &readOnlyDefault); err != nil {
		return err
	}
	if super {
		problems = append(problems, "read-only role is a superuser, so it can write")
	}
	if !readOnlyDefault {
		problems = append(problems, "read-only role's sessions are not read-only by default, so it could write")
	}
	if len(problems) > 0 {
		return fmt.Errorf("read-only SQL contract broken:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}
