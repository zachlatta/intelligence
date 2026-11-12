package db_test

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestPriorityMigrationPreservesRunSnapshots(t *testing.T) {
	d := dbtest.New(t)
	_, ann, _ := twoAccounts(t, d)
	for _, tc := range []struct{ name, definition, want string }{
		{"Email task", `{"trigger":"email"}`, "immediate"},
		{"Voice memos", `{"trigger":"voice_memo"}`, "immediate"},
		{"Task", `{"trigger":"task"}`, "normal"},
		{"Explicit normal", `{"trigger":"email","priority":"normal"}`, "normal"},
	} {
		var id string
		if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at) VALUES(1,$1,1,now(),now()) RETURNING id`, tc.name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, tc.definition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at) SELECT 1,id,1,'ann-host','Old request','Keep snapshot','queued',now(),now() FROM workflows WHERE name='Email task'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `DELETE FROM schema_migrations WHERE version='0045_workflow_priority'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"Email task": "immediate", "Voice memos": "immediate", "Task": "normal", "Explicit normal": "normal"} {
		var priority string
		if err := ann.QueryRow(ctx, `SELECT coalesce(v.definition->>'priority','normal') FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version WHERE w.name=$1`, name).Scan(&priority); err != nil || priority != want {
			t.Fatalf("%s: %q %v", name, priority, err)
		}
	}
	var version int
	var unchanged bool
	var priority string
	if err := ann.QueryRow(ctx, `SELECT r.version, v.definition='{"trigger":"email"}'::jsonb, r.priority FROM workflow_runs r JOIN workflow_versions v USING(workflow_id,version)`).Scan(&version, &unchanged, &priority); err != nil || version != 1 || !unchanged || priority != "normal" {
		t.Fatalf("snapshot: %d %v %v", version, unchanged, err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
