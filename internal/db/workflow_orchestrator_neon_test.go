package db_test

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestWorkflowOrchestratorNeonHighMigration(t *testing.T) {
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
		opusHighBefore = `{"host":"ann-host","trigger":"task","project":{"mode":"match","provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Find project."},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"high","instructions":"Use claude/claude-opus-5-5 for sub-agents."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		opusHighAfter  = `{"host":"ann-host","trigger":"task","project":{"mode":"match","provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Find project."},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-high","instructions":"Use claude/claude-opus-5-5 for sub-agents."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		opusNoThink    = `{"host":"ann-host","trigger":"email","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","instructions":"Handle email."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		opusNoThinkEnd = `{"host":"ann-host","trigger":"email","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-high","instructions":"Handle email."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		sonnetDef      = `{"host":"ann-host","trigger":"task","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"claude","model":"claude-sonnet-5-5","thinking":"medium","instructions":"Run."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		codexDef       = `{"host":"ann-host","trigger":"task","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"codex","model":"gpt-6.2-sol","thinking":"high","instructions":"Run."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
		neonMedDef     = `{"host":"ann-host","trigger":"voice_memo","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Handle memo."},"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work."}]}`
	)

	for _, tc := range []struct {
		name, definition string
		deleted          bool
	}{
		{"opus_high", opusHighBefore, false},
		{"opus_no_think", opusNoThink, false},
		{"sonnet", sonnetDef, false},
		{"codex", codexDef, false},
		{"neon_med", neonMedDef, false},
		{"deleted_opus", opusHighBefore, true},
	} {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at,deleted_at)
 VALUES(1,$1,1,now(),now(),CASE WHEN $2 THEN now() END) RETURNING id`, tc.name, tc.deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, tc.definition)
	}
	exec(`INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
 SELECT 1,id,1,'ann-host','Existing run','Hello','running',now(),now() FROM workflows WHERE name='opus_high'`)

	// Replay the migration over state saved before this release.
	exec(`DELETE FROM schema_migrations WHERE version='20261127T2310_workflow_orchestrator_neon_high'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{
		"opus_high":     2,
		"opus_no_think": 2,
		"sonnet":        1,
		"codex":         1,
		"neon_med":      1,
		"deleted_opus":  1,
	} {
		var version int
		if err := ann.QueryRow(ctx, `SELECT version FROM workflows WHERE name=$1`, name).Scan(&version); err != nil || version != want {
			t.Fatalf("%s version=%d, want %d: %v", name, version, want, err)
		}
	}
	for _, tc := range []struct {
		name, want string
		version    int
	}{
		{"opus_high", opusHighBefore, 1},
		{"opus_high", opusHighAfter, 2},
		{"opus_no_think", opusNoThink, 1},
		{"opus_no_think", opusNoThinkEnd, 2},
		{"sonnet", sonnetDef, 1},
		{"codex", codexDef, 1},
		{"neon_med", neonMedDef, 1},
		{"deleted_opus", opusHighBefore, 1},
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
	exec(`DELETE FROM schema_migrations WHERE version='20261127T2310_workflow_orchestrator_neon_high'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	var totalVersions int
	if err := ann.QueryRow(ctx, `SELECT count(*) FROM workflow_versions`).Scan(&totalVersions); err != nil || totalVersions != 8 {
		t.Fatalf("migration is not idempotent: total workflow_versions=%d, want 8 (err=%v)", totalVersions, err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
