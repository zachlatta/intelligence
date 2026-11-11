package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestRunSummaryIsStoredThroughTheAPIAndDisplayed(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "On the workflow runs page, make What a summary of the task rather than the first words.")
	before := readRun(t, h, run)
	if page := get(t, h, "/workflows", nil).Body.String(); !strings.Contains(page, before["title"].(string)) {
		t.Fatal("missing fallback title")
	}
	for _, invalid := range []string{"", " \n ", "two\nlines", strings.Repeat("x", 81)} {
		if code, msg := move(t, h, key, run, map[string]string{"move": "summarize", "summary": invalid}); code != http.StatusBadRequest {
			t.Fatalf("invalid summary: %d %s", code, msg)
		}
	}
	const summary = "Summarize workflow run titles"
	for _, denied := range []struct {
		key  string
		code int
	}{
		{"", http.StatusUnauthorized},
		{keyOf(t, h, "host-2"), http.StatusNotFound},
	} {
		if code, msg := move(t, h, denied.key, run, map[string]string{"move": "summarize", "summary": summary}); code != denied.code {
			t.Fatalf("unauthorized summary: %d %s", code, msg)
		}
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "summarize", "summary": "  " + summary + "  "}); code != http.StatusOK {
		t.Fatalf("summarize: %d %s", code, msg)
	}
	code, result := sqlPost(t, h, q("SELECT task_summary FROM workflow_runs WHERE id = '"+run+"'"))
	if code != http.StatusOK || len(result.Rows) != 1 || result.Rows[0][0] != summary {
		t.Fatalf("SQL summary: %d %+v", code, result)
	}
	for _, path := range []string{"/workflows", "/workflow-runs/" + run} {
		if page := get(t, h, path, nil).Body.String(); !strings.Contains(page, summary) {
			t.Errorf("%s missing summary", path)
		}
	}
	after := readRun(t, h, run)
	for _, field := range []string{"title", "status", "step", "branch", "workspace_title"} {
		if after[field] != before[field] {
			t.Errorf("summarizing changed %s", field)
		}
	}
	// Choosing the project recalculates the legacy title but must preserve the summary.
	if code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("project: %d %s", code, msg)
	}
	if page := get(t, h, "/workflows", nil).Body.String(); !strings.Contains(page, summary) {
		t.Fatal("project selection lost summary")
	}
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if code, _ := move(t, h, key, run, map[string]string{"move": "summarize", "summary": "Overwrite canceled run"}); code != http.StatusConflict {
		t.Fatalf("summarized canceled run: %d", code)
	}
}
