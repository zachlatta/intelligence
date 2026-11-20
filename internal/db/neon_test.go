package db_test

import (
	"slices"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestNeonMigrationAddsOnlyExtraHighToFrontier(t *testing.T) {
	d := dbtest.New(t)
	_, ann, bob := twoAccounts(t, d)
	exec := func(q string) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('antigravity','default',now(),now())`)
	exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('ann-host','antigravity','default',true,now())`)
	exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,first_observed,last_observed)
 VALUES('antigravity','default','gemini/7d',604800,10,now(),now())`)
	exec(`INSERT INTO model_groups(account_id,name,token) VALUES
 (1,'Renamed frontier','frontier_models_model'), (1,'Fast','fast_model'), (2,'Frontier Models','frontier_models_model')`)
	exec(`INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window,eligible_until)
 SELECT 1,id,3,'antigravity','gemini-4-carbon-high','','default','gemini/7d','2000-01-01'
 FROM model_groups WHERE account_id=1`)

	for range 2 {
		exec(`DELETE FROM schema_migrations WHERE version LIKE '%_neon_frontier'`)
		if err := db.Migrate(ctx, d.Owner); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"fast_model:1:3:gemini-4-carbon-high",
		"frontier_models_model:2:3:gemini-4-carbon-high",
		"frontier_models_model:2:4:gemini-4-neon-xhigh",
	}
	if got := strings1(t, ann, `SELECT concat_ws(':',g.token,g.revision,m.position,m.model)
 FROM model_groups g JOIN model_group_members m ON m.group_id=g.id ORDER BY g.token,m.position`); !slices.Equal(got, want) {
		t.Fatalf("model group members = %v, want %v", got, want)
	}
	if got := strings1(t, bob, `SELECT model FROM model_group_members`); len(got) != 0 {
		t.Fatalf("another account's group gained models without its own usage source: %v", got)
	}
	var guarded bool
	if err := ann.QueryRow(ctx, `SELECT login='default' AND thinking='' AND long_window='gemini/7d'
 AND enabled AND eligible_until IS NULL AND NOT eligible AND reason='Provider disabled'
 FROM model_group_candidates WHERE model='gemini-4-neon-xhigh'`).Scan(&guarded); err != nil || !guarded {
		t.Fatalf("Neon needs the Gemini meter and the provider gate: %v", err)
	}
}
