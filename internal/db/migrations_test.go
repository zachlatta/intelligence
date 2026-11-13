package db_test

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// The shipped migrations are all well named: legacy four-digit files up to 0049, then timestamps.
func TestShippedMigrationsAreWellNamed(t *testing.T) {
	names, err := db.ShippedMigrations()
	if err != nil {
		t.Fatalf("shipped migrations: %v", err)
	}
	if len(names) < 49 || names[0] != "0001_devboxes.sql" {
		t.Fatalf("shipped migrations = %d, first %q", len(names), names[0])
	}
}

func TestMigrationOrderPutsLegacyFirstThenTimestamps(t *testing.T) {
	got, err := db.MigrationOrder([]string{
		"20261113T0930_b.sql",
		"0049_voice_memo_poll_schedule.sql",
		"20261112T2359_a.sql",
		"0001_devboxes.sql",
		"20261113T0931_c.sql",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0001_devboxes.sql",
		"0049_voice_memo_poll_schedule.sql",
		"20261112T2359_a.sql",
		"20261113T0930_b.sql",
		"20261113T0931_c.sql",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestMigrationOrderRejectsBadNames(t *testing.T) {
	for name, wantErr := range map[string]string{
		"0050_new_thing.sql":       "timestamp",    // above the legacy range: use a timestamp
		"0100_new_thing.sql":       "timestamp",    // likewise
		"50_x.sql":                 "name",         // no recognised prefix
		"20261113_x.sql":           "name",         // date without time
		"20261113T930_x.sql":       "name",         // time too short
		"20261313T0930_x.sql":      "timestamp",    // month 13
		"20261113T2460_x.sql":      "timestamp",    // 24:60
		"20261113T0930_Bad.sql":    "name",         // capitals
		"20261113T0930-x.sql":      "name",         // no underscore
		"20261113T0930_.sql":       "name",         // no description
		"20261113T0930_x.sql.orig": "name",         // not .sql
		"0000_x.sql":               "legacy range", // 0 is not a legacy number
		"00491231T0000_x.sql":      "2026",         // would sort before the legacy files
		"20251231T2359_x.sql":      "2026",         // before the scheme existed
		"20261113T0930_x.SQL":      "name",         // upper-case extension
		"README.md":                "name",         // not a migration
	} {
		_, err := db.MigrationOrder([]string{"0001_devboxes.sql", name})
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, wantErr)
		}
	}
}

func TestMigrationOrderRejectsDuplicateLegacyNumbers(t *testing.T) {
	_, err := db.MigrationOrder([]string{"0040_a.sql", "0040_b.sql"})
	if err == nil || !strings.Contains(err.Error(), "same number") {
		t.Errorf("err = %v, want a duplicate-number error", err)
	}
}

// Two branches can write a migration in the same UTC minute: both are kept, ordered by full name.
func TestMigrationOrderAllowsEqualTimestamps(t *testing.T) {
	got, err := db.MigrationOrder([]string{"20261113T0930_b.sql", "20261113T0930_a.sql", "0049_x.sql"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0049_x.sql", "20261113T0930_a.sql", "20261113T0930_b.sql"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Production has recorded the legacy names (0001_... to 0049_...) as versions. After the deploy that
// brings the timestamp scheme, those rows still match, nothing re-runs, and only new files apply.
func TestLegacyRecordsUpgradeToTheTimestampScheme(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "DROP TABLE schema_migrations"); err != nil {
		t.Fatal(err)
	}
	// Each legacy file fails if it runs twice (CREATE TABLE without IF NOT EXISTS).
	before := fstest.MapFS{
		"0001_one.sql": {Data: []byte("CREATE TABLE mig_one (id int)")},
		"0049_two.sql": {Data: []byte("CREATE TABLE mig_two (id int)")},
	}
	if err := db.MigrateFrom(ctx, d.Owner, before); err != nil {
		t.Fatalf("old deploy: %v", err)
	}
	after := fstest.MapFS{
		"0001_one.sql":            before["0001_one.sql"],
		"0049_two.sql":            before["0049_two.sql"],
		"20261113T0930_three.sql": {Data: []byte("CREATE TABLE mig_three (id int)")},
	}
	if err := db.MigrateFrom(ctx, d.Owner, after); err != nil {
		t.Fatalf("new deploy: %v", err)
	}
	want := []string{"0001_one", "0049_two", "20261113T0930_three"}
	if got := strings1(t, d.Owner, "SELECT version FROM schema_migrations ORDER BY version"); !slices.Equal(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

// Two branches each add a migration. Branch B lands and deploys first; branch A's migration has the
// older timestamp. When A merges, neither file is renamed, and the next deploy applies A's.
func TestMigrationsFromParallelBranchesMergeWithoutRenaming(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "DROP TABLE schema_migrations"); err != nil {
		t.Fatal(err)
	}
	base := fstest.MapFS{"0049_base.sql": {Data: []byte("CREATE TABLE mig_base (id int)")}}
	branchA := "20261113T0930_branch_a.sql"
	branchB := "20261113T1015_branch_b.sql"

	mainAfterB := fstest.MapFS{
		"0049_base.sql": base["0049_base.sql"],
		branchB:         {Data: []byte("CREATE TABLE mig_b (id int)")},
	}
	if err := db.MigrateFrom(ctx, d.Owner, mainAfterB); err != nil {
		t.Fatalf("deploy with B: %v", err)
	}

	mainAfterA := fstest.MapFS{
		"0049_base.sql": base["0049_base.sql"],
		branchA:         {Data: []byte("CREATE TABLE mig_a (id int)")},
		branchB:         mainAfterB[branchB],
	}
	if err := db.MigrateFrom(ctx, d.Owner, mainAfterA); err != nil {
		t.Fatalf("deploy with A merged: %v", err)
	}
	want := []string{"0049_base", "20261113T0930_branch_a", "20261113T1015_branch_b"}
	if got := strings1(t, d.Owner, "SELECT version FROM schema_migrations ORDER BY version"); !slices.Equal(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
	for _, table := range []string{"mig_base", "mig_a", "mig_b"} {
		var ok bool
		if err := d.Owner.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing (err %v)", table, err)
		}
	}
}

// Two branches that each add a migration in the same UTC minute merge without renaming either.
func TestSameMinuteMigrationsFromParallelBranchesMergeWithoutRenaming(t *testing.T) {
	d := dbtest.New(t)
	if _, err := d.Owner.Exec(ctx, "DROP TABLE schema_migrations"); err != nil {
		t.Fatal(err)
	}
	branchA := "20261113T0930_widgets.sql"
	branchB := "20261113T0930_gadgets.sql"
	mainAfterA := fstest.MapFS{
		"0049_base.sql": {Data: []byte("CREATE TABLE mig_base (id int)")},
		branchA:         {Data: []byte("CREATE TABLE mig_widgets (id int)")},
	}
	if err := db.MigrateFrom(ctx, d.Owner, mainAfterA); err != nil {
		t.Fatalf("deploy with A: %v", err)
	}
	mainAfterB := fstest.MapFS{
		"0049_base.sql": mainAfterA["0049_base.sql"],
		branchA:         mainAfterA[branchA],
		branchB:         {Data: []byte("CREATE TABLE mig_gadgets (id int)")},
	}
	if err := db.MigrateFrom(ctx, d.Owner, mainAfterB); err != nil {
		t.Fatalf("deploy with B merged: %v", err)
	}
	want := []string{"0049_base", "20261113T0930_gadgets", "20261113T0930_widgets"}
	if got := strings1(t, d.Owner, "SELECT version FROM schema_migrations ORDER BY version"); !slices.Equal(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

// Every file in the migrations directory must be a migration: a stray x.SQL or x.sql.orig would
// otherwise be skipped without a word.
func TestMigrateRefusesFilesThatAreNotMigrations(t *testing.T) {
	for _, stray := range []string{"20261113T0930_x.SQL", "20261113T0930_x.sql.orig", "notes.txt"} {
		d := dbtest.New(t)
		err := db.MigrateFrom(ctx, d.Owner, fstest.MapFS{
			"0049_base.sql": {Data: []byte("CREATE TABLE mig_base (id int)")},
			stray:           {Data: []byte("CREATE TABLE mig_stray (id int)")},
		})
		if err == nil || !strings.Contains(err.Error(), stray) {
			t.Errorf("%s: err = %v, want one naming the stray file", stray, err)
		}
	}
}

// A badly named file stops the migration before anything runs.
func TestMigrateRefusesBadlyNamedFiles(t *testing.T) {
	d := dbtest.New(t)
	err := db.MigrateFrom(ctx, d.Owner, fstest.MapFS{
		"0050_too_new.sql": {Data: []byte("CREATE TABLE mig_too_new (id int)")},
	})
	if err == nil {
		t.Fatal("migrate accepted 0050_too_new.sql")
	}
	var exists bool
	if err := d.Owner.QueryRow(ctx, "SELECT to_regclass('mig_too_new') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("bad migration ran: exists=%v err=%v", exists, err)
	}
}
