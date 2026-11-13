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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Embed migrations/ so migrationNames can refuse stray files, not only read *.sql.
// Go embed ignores files and directories whose names start with . or _.
//
//go:embed migrations
var migrations embed.FS

// migrateLock serializes migrations across app instances starting at once.
const migrateLock = 7356216601

// Migrate applies the migrations not yet recorded in schema_migrations, in order (migrationOrder).
func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	return migrateFrom(ctx, p, migrationDir())
}

// migrationDir is the shipped migrations, rooted at internal/db/migrations.
func migrationDir() fs.FS {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // a constant, valid path
	}
	return dir
}

// migrateFrom applies the .sql files in dir not yet recorded in schema_migrations. Each is recorded by
// its name without ".sql" (its version), so a file that was applied is never run again, wherever it
// sorts. A file that sorts before ones already applied (an older timestamp merged from a parallel
// branch) is applied, not refused: parallel branches produce exactly that.
func migrateFrom(ctx context.Context, p *pgxpool.Pool, dir fs.FS) error {
	names, err := migrationNames(dir)
	if err != nil {
		return err
	}
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
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(dir, name)
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

// migrationNames lists the migrations in dir in the order they apply. Every entry in dir must be a
// well-named migration file: a stray one (x.SQL, x.sql.orig, notes.txt, a directory) is an error,
// not silently skipped.
func migrationNames(dir fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("migration directory holds a directory, %s: only .sql files belong there", e.Name())
		}
		names = append(names, e.Name())
	}
	return migrationOrder(names)
}

// legacyMigrations is the last four-digit migration number. Migrations 0001 to 0049 predate the
// timestamp scheme and keep their names, since production records them by name.
const legacyMigrations = 49

var (
	legacyName    = regexp.MustCompile(`^([0-9]{4})_[a-z0-9]+(?:_[a-z0-9]+)*\.sql$`)
	timestampName = regexp.MustCompile(`^([0-9]{8}T[0-9]{4})_[a-z0-9]+(?:_[a-z0-9]+)*\.sql$`)
)

// migrationOrder checks migration file names and sorts them into the order they apply.
//
// A new migration is named YYYYMMDDTHHMM_description.sql, the UTC time it was written (for example
// 20261113T0930_add_widgets.sql). Different descriptions distinguish migrations written in the same
// minute, so they need no renumbering when they merge. The legacy files 0001_... to 0049_... come
// first, in number order, then the timestamped ones in time order. Two with the same timestamp
// (perhaps from different branches) are both kept, ordered by full name. A four-digit number
// above 0049, two legacy files with the same number, a timestamp before 2026 or a malformed name is
// an error.
func migrationOrder(names []string) ([]string, error) {
	legacy := map[string]string{}
	for _, name := range names {
		if m := legacyName.FindStringSubmatch(name); m != nil {
			prefix := m[1]
			n, _ := strconv.Atoi(prefix)
			if n > legacyMigrations {
				return nil, fmt.Errorf("migration %s: four-digit numbers end at %04d; name a new migration with a UTC timestamp, like %s_%s",
					name, legacyMigrations, time.Now().UTC().Format(migrationTimestamp), strings.SplitN(name, "_", 2)[1])
			}
			if n < 1 {
				return nil, fmt.Errorf("migration %s: outside the legacy range 0001 to %04d", name, legacyMigrations)
			}
			if other, ok := legacy[prefix]; ok {
				return nil, fmt.Errorf("migrations %s and %s have the same number", other, name)
			}
			legacy[prefix] = name
		} else if m := timestampName.FindStringSubmatch(name); m != nil {
			at, err := time.Parse(migrationTimestamp, m[1])
			if err != nil {
				return nil, fmt.Errorf("migration %s: bad timestamp %s: %w", name, m[1], err)
			}
			if at.Year() < firstTimestampYear {
				return nil, fmt.Errorf("migration %s: timestamp before %d; use the current UTC time", name, firstTimestampYear)
			}
		} else {
			return nil, fmt.Errorf("migration %s: bad name; want YYYYMMDDTHHMM_lower_snake_description.sql (UTC)", name)
		}
	}
	// Fixed-width prefixes put legacy numbers first, then timestamps from 2026 onward by time,
	// with equal timestamps ordered by the rest of the name.
	out := slices.Clone(names)
	slices.Sort(out)
	return out, nil
}

// migrationTimestamp is the layout of a new migration's prefix (UTC).
const migrationTimestamp = "20060102T1504"

// firstTimestampYear is the earliest year a migration timestamp may have: the scheme began in 2026,
// and the bound keeps every timestamped name sorting after the legacy 0001 to 0049.
const firstTimestampYear = 2026

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
// possible. And that each account reads only its own rows: every table has row-level security
// (schema_migrations, which holds no account's rows, aside), every view reads as whoever queries
// it, and every account's role is read-only and bound by row-level security. It returns every
// failure it finds.
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
	problems = append(problems, scopeProblems(ctx, owner)...)
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
	var super, bypass, readOnlyDefault bool
	if err := readonly.QueryRow(ctx, "SELECT rolsuper, rolbypassrls, current_setting('default_transaction_read_only') = 'on' FROM pg_roles WHERE rolname = current_user").
		Scan(&super, &bypass, &readOnlyDefault); err != nil {
		return err
	}
	if super {
		problems = append(problems, "read-only role is a superuser, so it can write")
	}
	if bypass {
		problems = append(problems, "read-only role bypasses row-level security, so it reads every account's rows")
	}
	if !readOnlyDefault {
		problems = append(problems, "read-only role's sessions are not read-only by default, so it could write")
	}
	if len(problems) > 0 {
		return fmt.Errorf("read-only SQL contract broken:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}

// unscopedTables may hold rows every account reads: they hold no account's state.
var unscopedTables = []string{"schema_migrations"}

// scopeProblems is how an account could read rows that are not its own.
func scopeProblems(ctx context.Context, owner *pgxpool.Pool) []string {
	var problems []string
	rows, err := owner.Query(ctx, `SELECT c.relname, c.relkind::text, c.relrowsecurity,
		EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid AND p.polcmd IN ('r', '*')),
		coalesce('security_invoker=true' = ANY (c.reloptions) OR 'security_invoker=on' = ANY (c.reloptions), false)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'f') ORDER BY 1`)
	if err != nil {
		return []string{"cannot list relations: " + err.Error()}
	}
	type rel struct {
		name, kind             string
		rls, policy, asInvoker bool
	}
	rels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rel, error) {
		var v rel
		return v, r.Scan(&v.name, &v.kind, &v.rls, &v.policy, &v.asInvoker)
	})
	if err != nil {
		return []string{"cannot list relations: " + err.Error()}
	}
	for _, r := range rels {
		switch {
		case slices.Contains(unscopedTables, r.name):
		case r.kind == "v" && !r.asInvoker:
			problems = append(problems, fmt.Sprintf("view %s reads as its owner, past row-level security: set security_invoker", r.name))
		case r.kind == "m" || r.kind == "f":
			problems = append(problems, fmt.Sprintf("%s cannot be scoped to an account with row-level security: use a table or a view", r.name))
		case (r.kind == "r" || r.kind == "p") && (!r.rls || !r.policy):
			problems = append(problems, fmt.Sprintf("table %s has no row-level security policy, so every account would read all of it", r.name))
		}
	}
	rows, err = owner.Query(ctx, `SELECT a.role_name, r.rolname IS NOT NULL, coalesce(r.rolsuper OR r.rolbypassrls OR r.rolcreaterole, false),
		EXISTS (SELECT 1 FROM pg_db_role_setting s WHERE s.setrole = r.oid AND 'default_transaction_read_only=on' = ANY (s.setconfig))
		FROM accounts a LEFT JOIN pg_roles r ON r.rolname = a.role_name ORDER BY a.id`)
	if err != nil {
		return append(problems, "cannot list account roles: "+err.Error())
	}
	type role struct {
		name                       string
		exists, powerful, readOnly bool
	}
	roles, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (role, error) {
		var v role
		return v, r.Scan(&v.name, &v.exists, &v.powerful, &v.readOnly)
	})
	if err != nil {
		return append(problems, "cannot list account roles: "+err.Error())
	}
	for _, r := range roles {
		switch {
		case !r.exists: // made when its account first reads
		case r.powerful:
			problems = append(problems, fmt.Sprintf("account role %s is a superuser, bypasses row-level security or creates roles", r.name))
		case !r.readOnly:
			problems = append(problems, fmt.Sprintf("account role %s's sessions are not read-only by default, so it could write", r.name))
		}
	}
	return problems
}
