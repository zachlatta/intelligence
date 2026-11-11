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
	for _, invalid := range []string{"", " \n ", "two\nlines", "two\u2028lines", "two\u2029paragraphs", "hidden\u200btext", "spoof\u202etext", strings.Repeat("x", 81)} {
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
		page := get(t, h, path, nil).Body.String()
		if !strings.Contains(page, summary) {
			t.Errorf("%s missing summary", path)
		}
		if path != "/workflows" {
			cards, history, found := strings.Cut(page, "<h2>What happened</h2>")
			line := "Task summary: " + summary
			if !found || strings.Contains(cards, line) || !strings.Contains(history, line) {
				t.Error("summary event should appear only in What happened, not in the project card")
			}
			if !strings.Contains(cards, "<p>Started</p>") {
				t.Error("project card should still show the run's start event")
			}
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
	if code, _ := move(t, h, key, run, map[string]string{"move": "summarize", "summary": "Summarize canceled workflow run"}); code != http.StatusOK {
		t.Fatalf("could not label canceled run: %d", code)
	}
}

func TestSummaryLabelsEveryRunStatusWithoutMovingIt(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Please summarize historical tasks")
	for _, status := range []string{"finding_project", "starting", "running", "waiting", "succeeded", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status = $2 WHERE id = $1", run, status); err != nil {
				t.Fatal(err)
			}
			query := q("SELECT (to_jsonb(r) - 'task_summary' - 'updated_at')::text FROM workflow_runs r WHERE id = '" + run + "'")
			_, before := sqlPost(t, h, query)
			summary := "Summarize " + status + " workflow tasks"
			if code, msg := move(t, h, key, run, map[string]string{"move": "summarize", "summary": summary}); code != http.StatusOK {
				t.Fatalf("summary: %d %s", code, msg)
			}
			_, after := sqlPost(t, h, query)
			if len(before.Rows) != 1 || len(after.Rows) != 1 || before.Rows[0][0] != after.Rows[0][0] {
				t.Fatal("summary changed run state beyond its label and update time")
			}
			code, events := sqlPost(t, h, q("SELECT text, step, to_step FROM workflow_run_events WHERE run_id = '"+run+"' AND kind = 'summarize' ORDER BY id DESC LIMIT 1"))
			if code != http.StatusOK || len(events.Rows) != 1 || events.Rows[0][0] != summary || events.Rows[0][1] != nil || events.Rows[0][2] != nil {
				t.Fatalf("missing label event: %d %+v", code, events)
			}
		})
	}
}

func TestRunSummaryHeadingRefresh(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Keep the heading current")
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	live, heading, request := strings.Index(page, `<div id="live">`), strings.Index(page, "<h1>"), strings.Index(page, `<div class="request">`)
	if live < 0 || heading <= live || heading >= request || !strings.Contains(page, `["live", "steps"]`) {
		t.Fatal("heading must be inside the refreshed live block")
	}
	if !strings.Contains(page, "document.title = next.title") {
		t.Fatal("refresh must also update the document title")
	}
}
