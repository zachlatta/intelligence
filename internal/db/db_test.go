package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

var ctx = context.Background()

func TestMigrateIsIdempotent(t *testing.T) {
	d := dbtest.New(t)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := d.Owner.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n == 0 {
		t.Fatalf("schema_migrations count = %d, err = %v", n, err)
	}
}

func TestEnsureReadonlyRoleIsIdempotent(t *testing.T) {
	d := dbtest.New(t)
	if err := db.EnsureReadonlyRole(ctx, d.Owner, d.Role, "another-password"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
}

func TestContractHoldsOnAFreshDatabase(t *testing.T) {
	d := dbtest.New(t)
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatalf("contract: %v", err)
	}
}

func TestReadonlyRoleCannotWrite(t *testing.T) {
	d := dbtest.New(t)
	for _, q := range []string{
		"INSERT INTO schema_migrations (version) VALUES ('x')",
		"DELETE FROM schema_migrations",
		"CREATE TABLE sneaky (x int)",
		"SELECT set_config('role', 'postgres', false)",
	} {
		if _, err := d.Readonly.Exec(ctx, q); err == nil {
			t.Errorf("read-only role ran %q", q)
		}
	}
}

func TestContractFailsForATableTheReadonlyRoleCannotRead(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "CREATE TABLE hidden (x int); REVOKE SELECT ON hidden FROM "+d.Role); err != nil {
		t.Fatal(err)
	}
	err := db.CheckContract(ctx, d.Owner, d.Readonly)
	if err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("err = %v, want a failure naming the unreadable table", err)
	}
}

func TestContractFailsForStateOutsideThePublicSchema(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "CREATE SCHEMA side; CREATE TABLE side.stash (x int)"); err != nil {
		t.Fatal(err)
	}
	err := db.CheckContract(ctx, d.Owner, d.Readonly)
	if err == nil || !strings.Contains(err.Error(), "side.stash") {
		t.Fatalf("err = %v, want a failure naming the table outside public", err)
	}
}

func TestContractFailsWhenTheReadonlyRoleCanWrite(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "ALTER ROLE "+d.Role+" SET default_transaction_read_only = off; GRANT INSERT ON schema_migrations TO "+d.Role); err != nil {
		t.Fatal(err)
	}
	d2 := dbtest.Reconnect(t, d) // role settings apply to new sessions
	err := db.CheckContract(ctx, d2.Owner, d2.Readonly)
	if err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("err = %v, want a failure saying the read-only role can write", err)
	}
}

// twoAccounts makes accounts "ann" and "bob", each holding the key to its own box (ann-box and
// bob-box), each box with a workspace and a plan login, and returns each account's read pool.
func twoAccounts(t *testing.T, d dbtest.DB) (roles *db.AccountRoles, ann, bob *pgxpool.Pool) {
	t.Helper()
	roles = db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	pools := map[string]*pgxpool.Pool{}
	for i, name := range []string{"ann", "bob"} {
		id := int64(i + 1)
		role, err := roles.RoleName(id)
		if err != nil {
			t.Fatal(err)
		}
		box := name + "-box"
		if _, err := d.Owner.Exec(ctx, `INSERT INTO accounts (id, username, password_hash, role_name) VALUES ($1, $2, 'x', $3)`, id, name, role); err != nil {
			t.Fatal(err)
		}
		if err := roles.Ensure(ctx, d.Owner, role); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`INSERT INTO devbox_keys (box, account_id, key_hash, created_at) VALUES ($1, $2, $1 || '-hash', now())`,
			`INSERT INTO devboxes (name, last_seen) SELECT $1, now() WHERE $2::bigint > 0`,
			`INSERT INTO workspaces (box, id, name, kind, status, directory) SELECT $1, 'w', 'W', 'directory', 'done', '/w' WHERE $2::bigint > 0`,
			`INSERT INTO usage_accounts (provider, account, first_seen, last_seen) SELECT 'claude', $1 || '@example.com', now(), now() WHERE $2::bigint > 0`,
			`INSERT INTO usage_sources (box, provider, account, ok, observed_at, received_at) SELECT $1, 'claude', $1 || '@example.com', true, now(), now() WHERE $2::bigint > 0`,
		} {
			if _, err := d.Owner.Exec(ctx, q, box, id); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		p, err := roles.Pool(ctx, role)
		if err != nil {
			t.Fatal(err)
		}
		pools[name] = p
	}
	return roles, pools["ann"], pools["bob"]
}

func strings1(t *testing.T, p *pgxpool.Pool, q string) []string {
	t.Helper()
	rows, err := p.Query(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func TestAnAccountReadsOnlyItsOwnRows(t *testing.T) {
	d := dbtest.New(t)
	_, ann, bob := twoAccounts(t, d)
	for q, want := range map[string]string{
		"SELECT username FROM accounts":                 "ann",
		"SELECT box FROM devbox_keys":                   "ann-box",
		"SELECT name FROM devboxes":                     "ann-box",
		"SELECT name FROM devbox_status":                "ann-box",
		"SELECT box FROM workspaces":                    "ann-box",
		"SELECT box FROM workspace_status":              "ann-box",
		"SELECT account FROM usage_accounts":            "ann-box@example.com",
		"SELECT box FROM usage_sources":                 "ann-box",
		"SELECT current_account_id()::text":             "1",
		"SELECT count(*)::text FROM devbox_enrollments": "0",
	} {
		if got := strings1(t, ann, q); len(got) != 1 || got[0] != want {
			t.Errorf("ann: %s = %v, want [%s]", q, got, want)
		}
	}
	if got := strings1(t, bob, "SELECT name FROM devbox_status"); len(got) != 1 || got[0] != "bob-box" {
		t.Errorf("bob sees %v", got)
	}
	// the shared read-only role reads as no account: it sees no account's rows
	for _, q := range []string{"SELECT username FROM accounts", "SELECT name FROM devbox_status", "SELECT account FROM usage_accounts"} {
		if got := strings1(t, d.Readonly, q); len(got) != 0 {
			t.Errorf("read-only role: %s = %v, want none", q, got)
		}
	}
}

// TestAnAccountCannotReadAsAnother: the SQL API runs whatever an account sends, so nothing it can
// say in SQL reads another account's rows or writes.
func TestAnAccountCannotReadAsAnother(t *testing.T) {
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	for _, q := range []string{
		"SET ROLE " + d.Role,
		"RESET ROLE",
		"SET SESSION AUTHORIZATION DEFAULT",
		"SELECT set_config('row_security', 'off', false)",
	} {
		conn, err := ann.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Exec(ctx, q) // may fail; either way:
		var names []string
		rows, err := conn.Query(ctx, "SELECT name FROM devboxes")
		if err == nil {
			names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err == nil && slices.Contains(names, "bob-box") {
			t.Errorf("after %q ann reads bob's box", q)
		}
		conn.Release()
	}
	for _, q := range []string{
		"SET ROLE postgres",
		"UPDATE devbox_keys SET account_id = 1",
		"INSERT INTO devbox_keys (box, account_id, key_hash, created_at) VALUES ('x', 1, 'h', now())",
		"SET default_transaction_read_only = off; DELETE FROM devboxes",
	} {
		if _, err := ann.Exec(ctx, q); err == nil {
			t.Errorf("ann ran %q", q)
		}
	}
}

func TestAccountRolesAreIdempotentAndFollowTheReadonlyPassword(t *testing.T) {
	d := dbtest.New(t)
	roles, _, _ := twoAccounts(t, d)
	role, _ := roles.RoleName(1)
	if err := roles.Ensure(ctx, d.Owner, role); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if err := db.EnsureReadonlyRole(ctx, d.Owner, d.Role, "a-new-password"); err != nil {
		t.Fatal(err)
	}
	d2 := dbtest.ReconnectAs(t, d, d.Role, "a-new-password")
	fresh := db.NewAccountRoles(d2.Owner, d2.Readonly) // a restarted server, with the new password
	defer fresh.Close()
	p, err := fresh.Pool(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings1(t, p, "SELECT username FROM accounts"); len(got) != 1 || got[0] != "ann" {
		t.Errorf("after the password changed: %v", got)
	}
}

func TestContractFailsForATableWithoutRowLevelSecurity(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "CREATE TABLE open_to_all (x int)"); err != nil {
		t.Fatal(err)
	}
	err := db.CheckContract(ctx, d.Owner, d.Readonly)
	if err == nil || !strings.Contains(err.Error(), "open_to_all") {
		t.Fatalf("err = %v, want a failure naming the table without row-level security", err)
	}
}

func TestContractFailsForAViewThatReadsAsItsOwner(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "CREATE VIEW every_box AS SELECT name FROM devboxes"); err != nil {
		t.Fatal(err)
	}
	err := db.CheckContract(ctx, d.Owner, d.Readonly)
	if err == nil || !strings.Contains(err.Error(), "every_box") {
		t.Fatalf("err = %v, want a failure naming the view that bypasses row-level security", err)
	}
}

func TestContractFailsForAnAccountRoleThatCanWrite(t *testing.T) {
	d := dbtest.New(t)
	roles, _, _ := twoAccounts(t, d)
	role, _ := roles.RoleName(2)
	if _, err := d.Owner.Exec(ctx, "ALTER ROLE "+role+" SET default_transaction_read_only = off"); err != nil {
		t.Fatal(err)
	}
	err := db.CheckContract(ctx, d.Owner, d.Readonly)
	if err == nil || !strings.Contains(err.Error(), role) {
		t.Fatalf("err = %v, want a failure naming the writable account role", err)
	}
}

// TestAnAccountCannotChangeItsOwnRole: Postgres lets a role change its own password and its own
// setting defaults. Through the SQL API (one statement, in a read-only transaction) an account
// must do neither: a role made writable by default would stop the server at its next start.
func TestAnAccountCannotChangeItsOwnRole(t *testing.T) {
	d := dbtest.New(t)
	roles, ann, _ := twoAccounts(t, d)
	role, _ := roles.RoleName(1)
	for _, q := range []string{
		"ALTER ROLE " + role + " SET default_transaction_read_only = off",
		"ALTER ROLE " + role + " RESET ALL",
		"ALTER ROLE " + role + " PASSWORD 'mine now'",
		"ALTER ROLE CURRENT_USER SET statement_timeout = 0",
		"SELECT set_config('default_transaction_read_only', 'off', false)",
	} {
		tx, err := ann.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, q)
		_ = tx.Commit(ctx)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Errorf("after ann's attempts: %v", err)
	}
	fresh := db.NewAccountRoles(d.Owner, d.Readonly)
	defer fresh.Close()
	p, err := fresh.Pool(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings1(t, p, "SELECT username FROM accounts"); len(got) != 1 {
		t.Errorf("ann's role no longer logs in: %v", got)
	}
}
