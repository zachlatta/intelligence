package db_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// The production workflows the variants migration converts, by id and name.
const (
	emailTaskID = "08b5cc05-9fb2-4a96-8bef-1d9c5de89701"
	ultrafastID = "151dc53e-6a0c-4cb5-8e71-b156461a1c03"
)

func workflowVariantsMigration(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("migrations/*_workflow_model_variants.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("want one workflow_model_variants migration, have %v (%v)", files, err)
	}
	return strings.TrimSuffix(filepath.Base(files[0]), ".sql")
}

const (
	emailSteps  = `"steps":[{"id":"step-1","kind":"step","title":"New step","instructions":"Finish the workflow - that's it."}]`
	emailV13    = `{"host":"ann-host","trigger":"email","priority":"immediate","email":{"from":["zach@example.com"],"replies":"run"},"project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"An email was received. Reply to it."},` + emailSteps + `}`
	emailV14    = `{"host":"ann-host","trigger":"email","priority":"immediate","email":{"from":["zach@example.com"],"replies":"run"},"project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"An email was received. Reply to it. Finish approved checkouts, including any CAPTCHA."},` + emailSteps + `}`
	ultrafast   = `{"host":"ann-host","trigger":"email","priority":"immediate","email":{"from":["zach@example.com"],"replies":"run","to":"bot+fast@example.com"},"project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"codex","model":"gpt-6.2-sol","thinking":"high","serviceTier":"ultrafast","instructions":"An email was received at bot+fast. Reply to it."},` + emailSteps + `}`
	ultrafastTo = `{"host":"ann-host","trigger":"email","priority":"immediate","email":{"from":["zach@example.com"],"replies":"run","to":"bot+fast@example.com"},"project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"codex","model":"gpt-6.2-sol","thinking":"high","serviceTier":"ultrafast","instructions":"An email was received. Reply to it. Finish approved checkouts, including any CAPTCHA."},` + emailSteps + `}`
)

// seedEmailTasks writes Zach Email Task (v13, v14) and its Ultrafast copy (v1, with a run) as
// production has them, named source and copy.
func seedEmailTasks(t *testing.T, d dbtest.DB, source, copy string) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workflows(id,account_id,name,version,created_at,updated_at) VALUES($1,1,$2,14,now(),now())`, []any{emailTaskID, source}},
		{`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,13,1,$2,now()),($1,14,1,$3,now())`, []any{emailTaskID, emailV13, emailV14}},
		{`INSERT INTO workflows(id,account_id,name,version,created_at,updated_at) VALUES($1,1,$2,1,now(),now())`, []any{ultrafastID, copy}},
		{`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, []any{ultrafastID, ultrafast}},
		{`INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at) VALUES(1,$1,1,'ann-host','Earlier','Hello','succeeded',now(),now())`, []any{ultrafastID}},
	} {
		if _, err := d.Owner.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

func replayMigration(t *testing.T, d dbtest.DB, name string) {
	t.Helper()
	if _, err := d.Owner.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, name); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
}

func TestTheUltrafastEmailTaskBecomesAVariantOfZachEmailTask(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	seedEmailTasks(t, d, "Zach Email Task", "Zach Email Task (GPT-6.2 Sol Ultrafast)")
	migration := workflowVariantsMigration(t)

	check := func(round string) {
		t.Helper()
		var version int
		var of string
		var equal bool
		if err := ann.QueryRow(ctx, `SELECT w.version, coalesce(w.variant_of::text,''), v.definition = $2::jsonb FROM workflows w
			JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version WHERE w.id = $1`, ultrafastID, ultrafastTo).Scan(&version, &of, &equal); err != nil {
			t.Fatalf("%s: %v", round, err)
		}
		if version != 2 || of != emailTaskID || !equal {
			var got string
			_ = d.Owner.QueryRow(ctx, `SELECT definition::text FROM workflow_versions WHERE workflow_id = $1 ORDER BY version DESC LIMIT 1`, ultrafastID).Scan(&got)
			t.Fatalf("%s: Ultrafast v%d of %q, definition %s\nwant %s", round, version, of, got, ultrafastTo)
		}
		var kept, runs, sourceVersion int
		_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_versions WHERE workflow_id = $1 AND version = 1 AND definition = $2::jsonb`, ultrafastID, ultrafast).Scan(&kept)
		_ = d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE workflow_id = $1 AND version = 1`, ultrafastID).Scan(&runs)
		_ = d.Owner.QueryRow(ctx, `SELECT version FROM workflows WHERE id = $1`, emailTaskID).Scan(&sourceVersion)
		if kept != 1 || runs != 1 || sourceVersion != 14 {
			t.Fatalf("%s: v1 kept %d, runs on v1 %d, source v%d", round, kept, runs, sourceVersion)
		}
	}
	replayMigration(t, d, migration)
	check("first run")
	replayMigration(t, d, migration)
	check("replay")
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestTheVariantsMigrationLeavesRenamedWorkflowsAlone(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	twoAccounts(t, d)
	seedEmailTasks(t, d, "Zach Email Task", "Something else")
	replayMigration(t, d, workflowVariantsMigration(t))
	var version int
	var of string
	if err := d.Owner.QueryRow(ctx, `SELECT version, coalesce(variant_of::text,'') FROM workflows WHERE id = $1`, ultrafastID).Scan(&version, &of); err != nil {
		t.Fatal(err)
	}
	if version != 1 || of != "" {
		t.Fatalf("a renamed copy was converted: v%d of %q", version, of)
	}
}
