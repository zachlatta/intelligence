package db_test

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestSolMigrationUpgradesChoicesAndPreservesHistory(t *testing.T) {
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const original = `{"project":{"directory":"/srv/gpt-6.1-sol","provider":"codex","model":"gpt-6.1-sol","instructions":"Use GPT 6.1 Sol (High)."},"orchestrator":{"provider":"codex","model":"gpt-6.1-sol","thinking":"high","instructions":"Use GPT-6.1 Sol or codex/gpt-6.1-sol."},"steps":[{"instructions":"Review with GPT-6.1-Sol; keep {{frontier_models_model}}."}]}`
	const upgraded = `{"project":{"directory":"/srv/gpt-6.1-sol","provider":"codex","model":"gpt-6.2-sol","instructions":"Use GPT 6.2 Sol (High)."},"orchestrator":{"provider":"codex","model":"gpt-6.2-sol","thinking":"high","instructions":"Use GPT-6.2 Sol or codex/gpt-6.2-sol."},"steps":[{"instructions":"Review with GPT-6.2-Sol; keep {{frontier_models_model}}."}]}`
	for _, tc := range []struct {
		name, definition string
		deleted          bool
	}{
		{"sol", original, false},
		{"deleted", original, true},
		{"custom", `{"orchestrator":{"provider":"other","model":"gpt-6.1-sol"},"steps":[{"instructions":"Use gpt-6.1-terra."}]}`, false},
	} {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at,deleted_at)
 VALUES(1,$1,1,now(),now(),CASE WHEN $2 THEN now() END) RETURNING id`, tc.name, tc.deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, tc.definition)
	}
	exec(`INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
 SELECT 1,id,1,'ann-host','Existing run','Use gpt-6.1-sol','running',now(),now() FROM workflows WHERE name='sol'`)
	exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('codex','test-login',now(),now()),('other','test-login',now(),now())`)
	exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('ann-host','codex','test-login',true,now())`)
	exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
 VALUES('codex','test-login','7d',604800,30,now()+interval '1 day',now(),now())`)
	exec(`INSERT INTO usage_active(host,provider,account,since,until) VALUES('ann-host','codex','test-login',now(),now())`)
	exec(`UPDATE hosts SET providers='[{"name":"codex","available":true,"models":[{"id":"gpt-6.2-sol","thinking":["high","medium"]}]}]' WHERE name='ann-host'`)
	for _, token := range []string{"frontier_models_model", "codex_model", "deleted_model"} {
		var group string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO model_groups(account_id,name,token,deleted_at)
 VALUES(1,$1,$1,CASE WHEN $1='deleted_model' THEN now() END) RETURNING id`, token).Scan(&group); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window,enabled,eligible_until)
 VALUES(1,$1,0,'codex','gpt-6.1-sol','high','test-login','7d',true,'2099-01-01'),
 (1,$1,1,'codex','gpt-6.1-sol','medium','test-login','7d',false,NULL),
 (1,$1,2,'other','gpt-6.1-sol','high','test-login','7d',true,NULL),
 (1,$1,3,'codex','gpt-6.1-terra','high','test-login','7d',true,NULL)`, group)
	}
	// Replay just the upgrade over state saved before this release.
	exec(`DELETE FROM schema_migrations WHERE version='20261117T1841_sol_6_2'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"sol": 2, "deleted": 1, "custom": 1} {
		var version int
		if err := ann.QueryRow(ctx, `SELECT version FROM workflows WHERE name=$1`, name).Scan(&version); err != nil || version != want {
			t.Fatalf("%s version=%d, want %d: %v", name, version, want, err)
		}
	}
	for version, want := range map[int]string{1: original, 2: upgraded} {
		var equal bool
		if err := ann.QueryRow(ctx, `SELECT definition=$2::jsonb FROM workflow_versions v JOIN workflows w ON w.id=v.workflow_id WHERE w.name='sol' AND v.version=$1`, version, want).Scan(&equal); err != nil || !equal {
			t.Fatalf("workflow version %d changed unexpectedly: %v", version, err)
		}
	}
	var history bool
	if err := ann.QueryRow(ctx, `SELECT version=1 AND input='Use gpt-6.1-sol' FROM workflow_runs`).Scan(&history); err != nil || !history {
		t.Fatalf("run history changed: %v", err)
	}
	for _, token := range []string{"frontier_models_model", "codex_model", "deleted_model"} {
		var revision int
		if err := ann.QueryRow(ctx, `SELECT revision FROM model_groups WHERE token=$1`, token).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		wantRevision, wantModel := 2, "gpt-6.2-sol"
		if token == "deleted_model" {
			wantRevision, wantModel = 1, "gpt-6.1-sol"
		}
		if revision != wantRevision {
			t.Fatalf("%s revision=%d, want %d", token, revision, wantRevision)
		}
		var correct bool
		if err := ann.QueryRow(ctx, `SELECT bool_and(CASE position
 WHEN 0 THEN model=$2 AND thinking='high' AND login='test-login' AND long_window='7d' AND enabled AND eligible_until='2099-01-01'::timestamptz
 WHEN 1 THEN model=$2 AND thinking='medium' AND NOT enabled AND eligible_until IS NULL
 WHEN 2 THEN provider='other' AND model='gpt-6.1-sol'
 WHEN 3 THEN model='gpt-6.1-terra' END)
 FROM model_group_members m JOIN model_groups g ON g.id=m.group_id WHERE g.token=$1`, token, wantModel).Scan(&correct); err != nil || !correct {
			t.Fatalf("%s member settings changed unexpectedly: %v", token, err)
		}
	}
	// Even replaying the data migration cannot add versions or bump revisions again.
	exec(`DELETE FROM schema_migrations WHERE version='20261117T1841_sol_6_2'`)
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	var stable bool
	if err := ann.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_versions)=4 AND
 (SELECT revision FROM model_groups WHERE token='frontier_models_model')=2`).Scan(&stable); err != nil || !stable {
		t.Fatalf("upgrade is not idempotent: %v", err)
	}
	var eligible bool
	if err := ann.QueryRow(ctx, `SELECT eligible AND remaining_pct=70 AND long_window='7d' AND reason=''
 FROM model_group_candidates WHERE token='frontier_models_model' AND position=0`).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("new model did not retain its subscription meter: %v", err)
	}
	// Until Paseo advertises the new model, the migrated choice must fail closed.
	exec(`UPDATE hosts SET providers=replace(providers::text,'gpt-6.2-sol','gpt-6.1-sol')::jsonb WHERE name='ann-host'`)
	var unavailable bool
	if err := ann.QueryRow(ctx, `SELECT NOT eligible AND reason='Model or thinking unavailable'
 FROM model_group_candidates WHERE token='frontier_models_model' AND position=0`).Scan(&unavailable); err != nil || !unavailable {
		t.Fatalf("stale catalog did not exclude the new model: %v", err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
