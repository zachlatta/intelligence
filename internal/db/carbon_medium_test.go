package db_test

import (
	"slices"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestCarbonMediumMigrationRemovesOnlyFrontierMember(t *testing.T) {
	d := dbtest.New(t)
	_, ann, bob := twoAccounts(t, d)
	exec := func(q string) {
		t.Helper()
		if _, err := d.Owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO model_groups(account_id,name,token,revision,deleted_at) VALUES
 (1,'Renamed frontier','frontier_models_model',4,NULL),
 (1,'Other','other_model',1,NULL),
 (2,'Archived frontier','frontier_models_model',3,now());
 INSERT INTO usage_accounts(provider,account,first_seen,last_seen)
 VALUES('antigravity','default',now(),now());
 INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window)
 SELECT account_id,id,1,'antigravity','gemini-4-carbon-medium','','default','gemini/7d'
 FROM model_groups;
 INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window,enabled)
 SELECT account_id,id,member.position,'antigravity',member.model,'','default','gemini/7d',member.enabled
 FROM model_groups CROSS JOIN (VALUES
    (0,'gemini-4-carbon-high',false), (2,'gemini-4-neon-medium',true)
 ) AS member(position,model,enabled)
 WHERE account_id=1 AND token='frontier_models_model'`)

	const members = `SELECT to_jsonb(m)::text FROM model_group_members m
 JOIN model_groups g ON g.id=m.group_id`
	const order = ` ORDER BY g.token,m.position`
	wantAnn := strings1(t, ann, members+` WHERE g.token<>'frontier_models_model' OR m.model<>'gemini-4-carbon-medium'`+order)
	wantBob := strings1(t, bob, members+order)

	for range 2 {
		exec(`DELETE FROM schema_migrations WHERE version LIKE '%_remove_carbon_medium'`)
		if err := db.Migrate(ctx, d.Owner); err != nil {
			t.Fatal(err)
		}
	}

	if got := strings1(t, ann, `SELECT m.model FROM model_group_members m JOIN model_groups g ON g.id=m.group_id
 WHERE g.token='frontier_models_model' ORDER BY m.position`); !slices.Equal(got, []string{"gemini-4-carbon-high", "gemini-4-neon-medium"}) {
		t.Errorf("frontier members = %v, want Carbon High and Neon Medium", got)
	}
	if got := strings1(t, ann, members+order); !slices.Equal(got, wantAnn) {
		t.Error("retained members of active groups should keep their original rows and positions")
	}
	if got := strings1(t, bob, members+order); !slices.Equal(got, wantBob) {
		t.Error("members of the archived group should keep their original rows")
	}
	const revisions = `SELECT token || ':' || revision FROM model_groups ORDER BY token`
	if got := strings1(t, ann, revisions); !slices.Equal(got, []string{"frontier_models_model:5", "other_model:1"}) {
		t.Errorf("active group revisions = %v, want a single bump for frontier only", got)
	}
	if got := strings1(t, bob, revisions); !slices.Equal(got, []string{"frontier_models_model:3"}) {
		t.Errorf("archived group revision = %v, want 3", got)
	}
}
