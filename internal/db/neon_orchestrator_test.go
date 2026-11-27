package db_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// neonOrchestratorMigration is the one migration that moves saved Opus orchestrators to Neon
// Medium, found by name so a rename before landing does not break the test.
func neonOrchestratorMigration(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("migrations/*_workflow_orchestrator_neon*.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("want one workflow_orchestrator_neon migration, have %v (%v)", files, err)
	}
	return strings.TrimSuffix(filepath.Base(files[0]), ".sql")
}

func TestOpusWorkflowOrchestratorsMoveToNeonMedium(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	_, ann, bob := twoAccounts(t, d)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const steps = `"steps":[{"id":"do","kind":"step","title":"Do","instructions":"Work. Hire a reviewer on Opus 5.5."}]`
	const (
		opusHigh   = `{"trigger":"task","project":{"mode":"match","provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Find project."},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"high","instructions":"Run the steps."},` + steps + `}`
		opusHighTo = `{"trigger":"task","project":{"mode":"match","provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Find project."},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Run the steps."},` + steps + `}`
		opusMedium = `{"trigger":"email","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Handle email."},` + steps + `}`
		opusMedTo  = `{"trigger":"email","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Handle email."},` + steps + `}`
		codex      = `{"trigger":"task","project":{"mode":"match","provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"Find."},"orchestrator":{"provider":"codex","model":"gpt-6.2-sol","thinking":"high","instructions":"Run."},` + steps + `}`
		neonMed    = `{"trigger":"voice_memo","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"gemini-4-neon-medium","instructions":"Memo."},` + steps + `}`
		sonnet     = `{"trigger":"task","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"claude","model":"claude-sonnet-5-5","thinking":"high","instructions":"Run."},` + steps + `}`
		agOpus     = `{"trigger":"task","project":{"mode":"fixed","directory":"/srv/desk"},"orchestrator":{"provider":"antigravity","model":"claude-opus-5-5","instructions":"Run."},` + steps + `}`
	)
	type wf struct {
		account          int
		name, definition string
		deleted          bool
		want             string // the new current definition; "" when it must not change
	}
	cases := []wf{
		{1, "opus_high", opusHigh, false, opusHighTo},
		{1, "opus_medium", opusMedium, false, opusMedTo},
		{2, "bob_opus", opusHigh, false, opusHighTo},
		{1, "codex", codex, false, ""},
		{1, "neon_medium", neonMed, false, ""},
		{1, "sonnet", sonnet, false, ""},
		{1, "ag_opus", agOpus, false, ""},
		{1, "deleted", opusHigh, true, ""},
	}
	for _, c := range cases {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at,deleted_at)
 VALUES($1,$2,1,now(),now(),CASE WHEN $3 THEN now() END) RETURNING id`, c.account, c.name, c.deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,$2,$3,now())`, id, c.account, c.definition)
	}
	exec(`INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
 SELECT 1,id,1,'ann-host','Existing run','Hello','running',now(),now() FROM workflows WHERE name='opus_high'`)

	migration := neonOrchestratorMigration(t)
	var applied int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = $1`, migration).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("migration %s is not applied", migration)
	}
	replay := func() {
		t.Helper()
		exec(`DELETE FROM schema_migrations WHERE version = $1`, migration)
		if err := db.Migrate(ctx, d.Owner); err != nil {
			t.Fatal(err)
		}
	}
	replay()

	check := func(round string) {
		t.Helper()
		for _, c := range cases {
			pool := ann
			if c.account == 2 {
				pool = bob
			}
			wantVersion, want := 1, c.definition
			if c.want != "" {
				wantVersion, want = 2, c.want
			}
			var version int
			var equal bool
			if err := pool.QueryRow(ctx, `SELECT w.version, v.definition = $2::jsonb FROM workflows w
 JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version WHERE w.name = $1`, c.name, want).Scan(&version, &equal); err != nil {
				t.Fatalf("%s %s: %v", round, c.name, err)
			}
			if version != wantVersion || !equal {
				var got string
				_ = d.Owner.QueryRow(ctx, `SELECT v.definition::text FROM workflows w
 JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version WHERE w.name = $1`, c.name).Scan(&got)
				t.Errorf("%s %s: version %d (want %d), definition = %s\nwant %s", round, c.name, version, wantVersion, got, want)
			}
			if c.want != "" {
				var kept bool
				if err := pool.QueryRow(ctx, `SELECT v.definition = $2::jsonb FROM workflows w
 JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = 1 WHERE w.name = $1`, c.name, c.definition).Scan(&kept); err != nil || !kept {
					t.Errorf("%s %s: version 1 was rewritten (%v)", round, c.name, err)
				}
			}
		}
		var runVersion int
		if err := ann.QueryRow(ctx, `SELECT version FROM workflow_runs`).Scan(&runVersion); err != nil || runVersion != 1 {
			t.Errorf("%s: existing run moved to version %d (%v)", round, runVersion, err)
		}
	}
	check("first run")
	// Replaying is a no-op: no third version.
	replay()
	check("replay")
	var total int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_versions`).Scan(&total); err != nil || total != len(cases)+3 {
		t.Errorf("workflow_versions = %d, want %d (%v)", total, len(cases)+3, err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
