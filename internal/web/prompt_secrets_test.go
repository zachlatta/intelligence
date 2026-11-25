package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowRefusesCredentialNotesRepliesAndLaterSteps(t *testing.T) {
	t.Parallel()
	h, db := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	d := workflow.Starting
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, h, createWorkflow(t, h, d), "Fix tests")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	token := "pdw_" + strings.Repeat("a", 12) + "_" + strings.Repeat("x", 40)
	for _, action := range []map[string]string{
		{"move": "ask", "note": token}, {"move": "back", "why": token, "step": "build"}, {"move": "next", "summary": token},
		// claims reach later runs' prompts; status lines and steer targets are agent text too
		{"move": "claim", "deliverable": token}, {"move": "status", "now": token}, {"move": "status", "now": "Building", "waitingOn": token},
		{"move": "status", "now": "Building", "eta": token}, {"move": "steer", "into": token, "note": "same deck"},
	} {
		code, msg := move(t, h, key, run, action)
		if code == http.StatusOK || strings.Contains(msg, token) {
			t.Error("credential move was accepted or disclosed")
		}
	}
	if readRun(t, h, run)["status"] != "running" {
		t.Fatal("refused move changed run status")
	}
	if code, _ := move(t, h, key, run, map[string]string{"move": "ask", "note": "Continue?"}); code != http.StatusOK {
		t.Fatal("safe ask failed")
	}
	raw, _ := json.Marshal(map[string]string{"text": token})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", string(raw), sameOriginFetch)
	if rec.Code == http.StatusNoContent || strings.Contains(rec.Body.String(), token) {
		t.Error("credential reply accepted or disclosed")
	}
	if readRun(t, h, run)["status"] != "waiting" {
		t.Fatal("refused reply changed run status")
	}
	// A saved older workflow can acquire a bad later-step instruction. A failed
	// render must roll back its move/event, rather than advancing without a brief.
	if _, err := db.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='running' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Owner.Exec(t.Context(), `UPDATE workflow_versions SET definition=jsonb_set(definition,'{steps,1,instructions}',to_jsonb($2::text)) WHERE workflow_id=(SELECT workflow_id FROM workflow_runs WHERE id=$1)`, run, token); err != nil {
		t.Fatal(err)
	}
	code, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "Done"})
	if code == http.StatusOK || strings.Contains(msg, token) {
		t.Error("later-step credential was accepted or disclosed")
	}
	if readRun(t, h, run)["step"] != float64(0) {
		t.Error("refused later step advanced the run")
	}
}
