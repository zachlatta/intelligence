package web

import (
	"os"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestMain(m *testing.M) {
	os.Exit(dbtest.RunWithTemplate(m))
}

func TestDatabaseFixturesAreIsolated(t *testing.T) {
	t.Parallel()
	first := dbtest.New(t)
	fixtureFor(t, first)
	second := dbtest.New(t)
	var firstMigration, secondMigration time.Time
	query := "SELECT min(applied_at) FROM schema_migrations"
	if err := first.Owner.QueryRow(t.Context(), query).Scan(&firstMigration); err != nil {
		t.Fatal(err)
	}
	if err := second.Owner.QueryRow(t.Context(), query).Scan(&secondMigration); err != nil {
		t.Fatal(err)
	}
	if !firstMigration.Equal(secondMigration) {
		t.Errorf("fixtures reapplied migrations (%v and %v)", firstMigration, secondMigration)
	}
	var accounts int
	if err := second.Owner.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("fresh database has %d accounts, error %v", accounts, err)
	}
	if first.Name == second.Name || first.Role == second.Role {
		t.Fatal("test databases and reader roles must be distinct")
	}
	if err := db.CheckContract(t.Context(), second.Owner, second.Readonly); err != nil {
		t.Fatal(err)
	}
}
