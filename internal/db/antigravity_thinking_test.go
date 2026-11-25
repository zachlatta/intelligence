package db_test

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestClearAntigravityWorkflowThinkingMigration(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	const (
		emailBefore = `{"trigger":"email","project":{"mode":"fixed","directory":"/srv/desk","provider":"claude","model":"claude-opus-5-5","thinking":"medium"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","thinking":"medium","instructions":"Handle email."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		emailAfter  = `{"trigger":"email","project":{"mode":"fixed","directory":"/srv/desk","provider":"claude","model":"claude-opus-5-5","thinking":"medium"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Handle email."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		projBefore  = `{"trigger":"task","project":{"mode":"match","provider":"antigravity","model":"gemini-4-neon-medium","thinking":"high","instructions":"Find project."},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Run."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		projAfter   = `{"trigger":"task","project":{"mode":"match","provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Find project."},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Run."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		claudeDef   = `{"trigger":"task","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Run."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		cleanAG     = `{"trigger":"voice_memo","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Handle memo."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
	)

	for _, tc := range []struct {
		name, definition string
		deleted          bool
	}{
		{"email", emailBefore, false},
		{"proj", projBefore, false},
		{"claude", claudeDef, false},
		{"clean_ag", cleanAG, false},
		{"deleted", emailBefore, true},
	} {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at,deleted_at)
 VALUES(1,$1,1,now(),now(),CASE WHEN $2 THEN now() END) RETURNING id`, tc.name, tc.deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, tc.definition)
	}
	exec(`INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
 SELECT 1,id,1,'ann-host','Existing run','Hello','running',now(),now() FROM workflows WHERE name='email'`)

	// Replay the migration over state saved before this release.
	exec(`DELETE FROM schema_migrations WHERE version='20261122T0719_clear_antigravity_workflow_thinking'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{"email": 2, "proj": 2, "claude": 1, "clean_ag": 1, "deleted": 1} {
		var version int
		if err := ann.QueryRow(ctx, `SELECT version FROM workflows WHERE name=$1`, name).Scan(&version); err != nil || version != want {
			t.Fatalf("%s version=%d, want %d: %v", name, version, want, err)
		}
	}
	for _, tc := range []struct {
		name, want string
		version    int
	}{
		{"email", emailBefore, 1},
		{"email", emailAfter, 2},
		{"proj", projBefore, 1},
		{"proj", projAfter, 2},
	} {
		var equal bool
		if err := ann.QueryRow(ctx, `SELECT definition=$3::jsonb FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id WHERE w.name=$1 AND v.version=$2`, tc.name, tc.version, tc.want).Scan(&equal); err != nil || !equal {
			t.Fatalf("%s v%d definition mismatch: %v", tc.name, tc.version, err)
		}
	}
	var history bool
	if err := ann.QueryRow(ctx, `SELECT version=1 FROM workflow_runs`).Scan(&history); err != nil || !history {
		t.Fatalf("run history changed: %v", err)
	}

	// Replaying the migration again must be a no-op.
	exec(`DELETE FROM schema_migrations WHERE version='20261122T0719_clear_antigravity_workflow_thinking'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	var totalVersions int
	if err := ann.QueryRow(ctx, `SELECT count(*) FROM workflow_versions`).Scan(&totalVersions); err != nil || totalVersions != 7 {
		t.Fatalf("migration is not idempotent: total workflow_versions=%d, want 7 (err=%v)", totalVersions, err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
