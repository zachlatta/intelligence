package db_test

import (
	"encoding/json"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestCarbonMigrationVersionsSavedWorkflows(t *testing.T) {
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	const original = `{"project":{"directory":"/srv/gemini-4-argon-high"},"orchestrator":{"provider":"antigravity","model":"gemini-4-argon-high","instructions":"Use Gemini 4 Argon and antigravity/gemini-4-argon-medium."},"steps":[{"instructions":"Review with gemini-4-argon-low."}]}`
	const oldDefault = `{"orchestrator":{"instructions":"Use frontier models, and spread the work across the plans: claude/claude-opus-5-5 and codex."}}`
	for _, tc := range []struct {
		name, definition string
		deleted          bool
	}{
		{"argon", original, false}, {"default", oldDefault, false},
		{"custom", `{"orchestrator":{"instructions":"Use my chosen model."}}`, false},
		{"deleted", original, true},
	} {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at,deleted_at)
 VALUES(1,$1,1,now(),now(),CASE WHEN $2 THEN now() END) RETURNING id`, tc.name, tc.deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, tc.definition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
 SELECT 1,id,1,'ann-host','Existing run','Keep Argon context','running',now(),now() FROM workflows WHERE name='argon'`); err != nil {
		t.Fatal(err)
	}
	// Re-run only the new migration over definitions saved before the upgrade.
	if _, err := d.Owner.Exec(ctx, `DELETE FROM schema_migrations WHERE version='0027_carbon_frontier'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"argon": 2, "default": 2, "custom": 1, "deleted": 1} {
		var version int
		if err := d.Owner.QueryRow(ctx, `SELECT version FROM workflows WHERE name=$1`, name).Scan(&version); err != nil || version != want {
			t.Fatalf("%s version=%d, want %d: %v", name, version, want, err)
		}
	}
	var runVersion int
	if err := ann.QueryRow(ctx, `SELECT version FROM workflow_runs`).Scan(&runVersion); err != nil || runVersion != 1 {
		t.Fatalf("existing run version=%d: %v", runVersion, err)
	}
	want := `{"project":{"directory":"/srv/gemini-4-argon-high"},"orchestrator":{"provider":"antigravity","model":"gemini-4-carbon-high","instructions":"Use Gemini 4 Carbon and antigravity/gemini-4-carbon-medium."},"steps":[{"instructions":"Review with gemini-4-carbon-low."}]}`
	for version, expected := range map[int]string{1: original, 2: want} {
		var equal bool
		if err := ann.QueryRow(ctx, `SELECT definition=$2::jsonb FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id WHERE w.name='argon' AND v.version=$1`, version, expected).Scan(&equal); err != nil || !equal {
			t.Fatalf("argon version %d differs: %v", version, err)
		}
	}
	var definition []byte
	if err := ann.QueryRow(ctx, `SELECT definition FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id WHERE w.name='default' AND v.version=2`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	var def struct {
		Orchestrator struct{ Instructions string }
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Orchestrator.Instructions != "Use frontier models, and spread the work across the plans: claude/claude-opus-5-5, codex and antigravity/gemini-4-carbon-high (Gemini 4 Carbon)." {
		t.Fatalf("default instructions = %q", def.Orchestrator.Instructions)
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_versions`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("migration repeated: versions=%d: %v", count, err)
	}
}
