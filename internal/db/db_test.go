package db_test

import (
	"context"
	"strings"
	"testing"

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
