package db_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestStepModelMigrationPreservesInstructions(t *testing.T) {
	d := dbtest.New(t)
	twoAccounts(t, d)
	const original = `{"project":{"model":"finder","thinking":"high"},"orchestrator":{"model":"orch"},"steps":[{"instructions":"Build.","model":"{{frontier_model}}"},{"instructions":"Use {{frontier_model}} already.","model":"{{frontier_model}}"},{"instructions":"Review.","model":"codex/sol","thinking":"high"},{"instructions":"Unchanged."},{"instructions":"Empty.","model":"","thinking":""}]}`
	var id string
	if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows(account_id,name,version,created_at,updated_at) VALUES(1,'models',1,now(),now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_versions(workflow_id,version,account_id,definition,created_at) VALUES($1,1,1,$2,now())`, id, original); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_proposals(account_id,proposer,operation,workflow_id,summary,before_state,proposed_state,after_state,undo_state,created_at,expires_at)
 VALUES(1,'test','update',$1,'models',jsonb_build_object('definition',$2::jsonb),jsonb_build_object('definition',$2::jsonb),jsonb_build_object('definition',$2::jsonb),jsonb_build_object('definition',$2::jsonb),now(),now()+interval '1 day')`, id, original); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `DELETE FROM schema_migrations WHERE version='0044_remove_step_models'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := d.Owner.QueryRow(ctx, `SELECT definition FROM workflow_versions WHERE workflow_id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var def struct {
		Project      map[string]string
		Orchestrator map[string]string
		Steps        []map[string]string
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	for i, step := range def.Steps {
		if _, ok := step["model"]; ok {
			t.Errorf("step %d retains model", i)
		}
		if _, ok := step["thinking"]; ok {
			t.Errorf("step %d retains thinking", i)
		}
	}
	if !strings.Contains(def.Steps[0]["instructions"], "{{frontier_model}}") || strings.Count(def.Steps[1]["instructions"], "{{frontier_model}}") != 1 ||
		!strings.Contains(def.Steps[2]["instructions"], "codex/sol") || !strings.Contains(def.Steps[2]["instructions"], "high") || def.Steps[3]["instructions"] != "Unchanged." || def.Steps[4]["instructions"] != "Empty." {
		t.Fatalf("lost model intent: %s", raw)
	}
	if def.Project["model"] != "finder" || def.Project["thinking"] != "high" || def.Orchestrator["model"] != "orch" {
		t.Fatal("changed agent settings")
	}
	for _, field := range []string{"before_state", "proposed_state", "after_state", "undo_state"} {
		var equal bool
		if err := d.Owner.QueryRow(ctx, `SELECT `+field+`->'definition'=$1::jsonb FROM workflow_proposals`, raw).Scan(&equal); err != nil || !equal {
			t.Fatalf("%s not converted: %v", field, err)
		}
	}
	if err := db.Migrate(ctx, d.Owner); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
