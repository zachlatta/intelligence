package db_test

import (
	"slices"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestNeonMediumMigrationRestoresAntigravityAndReplacesFrontierMember(t *testing.T) {
	d := dbtest.New(t)
	_, ann, bob := twoAccounts(t, d)
	exec := func(q string) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE provider_plan_settings SET enabled=false WHERE provider='antigravity';
 INSERT INTO model_groups(account_id,name,token) VALUES
 (1,'Renamed frontier','frontier_models_model'), (1,'Other','other_model'),
 (2,'Frontier Models','frontier_models_model');
 INSERT INTO usage_accounts(provider,account,first_seen,last_seen)
 VALUES('antigravity','default',now(),now());
 INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window)
 SELECT account_id,id,3,'antigravity','gemini-4-neon-xhigh','','default','gemini/7d' FROM model_groups;
 INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window,enabled)
 SELECT account_id,id,5,'antigravity','gemini-4-neon-medium','','default','gemini/7d',false
 FROM model_groups WHERE account_id=2;
 INSERT INTO usage_sources(host,provider,account,ok,received_at)
 VALUES('ann-host','antigravity','default',true,now());
 INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
 VALUES('antigravity','default','gemini/7d',604800,10,now()+interval '5 days',now(),now());
 INSERT INTO usage_active(host,provider,account,since,until)
 VALUES('ann-host','antigravity','default',now(),now());
 UPDATE hosts SET providers='[{"name":"antigravity","available":true,"models":[{"id":"gemini-4-neon-medium"}]}]'
 WHERE name='ann-host'`)

	for range 2 {
		exec(`DELETE FROM schema_migrations WHERE version LIKE '%_restore_antigravity_neon_medium'`)
		if err := db.Migrate(ctx, d.Owner); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings1(t, ann, `SELECT provider FROM provider_plan_settings WHERE NOT enabled ORDER BY provider`); !slices.Equal(got, []string{"gemini", "muse"}) {
		t.Errorf("disabled providers = %v, want Gemini and Muse", got)
	}
	const members = `SELECT concat_ws(':',g.token,g.revision,m.position,m.model,m.enabled)
 FROM model_groups g JOIN model_group_members m ON m.group_id=g.id ORDER BY g.token,m.position`
	if got := strings1(t, ann, members); !slices.Equal(got, []string{
		"frontier_models_model:2:3:gemini-4-neon-medium:t",
		"other_model:1:3:gemini-4-neon-xhigh:t",
	}) {
		t.Errorf("frontier and other group members = %v", got)
	}
	if got := strings1(t, bob, members); !slices.Equal(got, []string{"frontier_models_model:2:5:gemini-4-neon-medium:f"}) {
		t.Errorf("preexisting Medium selection = %v", got)
	}
	var eligible bool
	if err := ann.QueryRow(ctx, `SELECT eligible AND thinking='' AND login='default' AND long_window='gemini/7d'
 FROM model_group_candidates WHERE token='frontier_models_model' AND model='gemini-4-neon-medium'`).Scan(&eligible); err != nil || !eligible {
		t.Errorf("Medium should be eligible with the reported catalog and meter: eligible=%t, error=%v", eligible, err)
	}
}
