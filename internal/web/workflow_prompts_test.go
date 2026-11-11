package web

import (
	"html"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowPromptsPage(t *testing.T) {
	h, _ := dbServer(t)
	rep := wfReport()
	// a git project without a workspace is offered to the finder; a plain folder is not
	rep.Paseo.Projects = []hostclient.Project{
		{ID: "p-empty", Name: "files", Kind: "git", Directory: "/w/files"},
		{ID: "p-dir", Name: "folder", Kind: "non_git", Directory: "/w/folder"},
	}
	sendReport(t, h, rep)
	d := workflow.Starting
	id := createWorkflow(t, h, d)
	path := "/workflows/" + id + "/prompts"
	for _, page := range []string{"/workflows", "/workflows/" + id} {
		if r := get(t, h, page, nil); !strings.Contains(r.Body.String(), `href="`+path+`"`) {
			t.Errorf("%s has no prompt inspection link", page)
		}
	}
	r := get(t, h, path, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("prompts: %d %s", r.Code, r.Body)
	}
	body := r.Body.String()
	for _, want := range []string{
		"<title>Prompts · Task from Intelligence · Intelligence</title>",
		"Step N is done.",
		"Back to step N",
		"automatically denies",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompts page missing %q", want)
		}
	}
	if _, mainBody, ok := strings.Cut(body, "<main"); !ok || strings.Contains(mainBody, "<style") {
		t.Errorf("styles should live in <head>, not inside <main>")
	}
	v := workflow.Vars{Run: "{{run}}", Input: "{{input}}", Repo: "{{repo}}", Branch: "{{branch}}"}
	wants := []string{workflow.ProjectPrompt(v.Run, d, v.Input, []string{"/w/app", "/w/files"}),
		workflow.OrchestratorPrompt("Task from Intelligence", d, v)}
	for i := range d.Steps {
		wants = append(wants, workflow.StepMessage(d, i, v))
	}
	for _, want := range wants {
		if !strings.Contains(body, "<pre>"+html.EscapeString(want)+"</pre>") {
			t.Errorf("missing exact composed message: %.100s", want)
		}
	}
	if strings.Contains(body, "/w/folder") {
		t.Errorf("non-git project directory included in finder preview")
	}
	// Saved edits, rather than the starting definition, must drive the preview; optional model/thinking
	// omit dangling separators, and fixed-repository workflows fill {{repo}} while keeping run placeholders.
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	d.Orchestrator = workflow.Orchestrator{Provider: "codex", Instructions: "Custom <script>alert('prompt')</script>"}
	d.Steps = slices.Clone(d.Steps)
	d.Steps[0].Instructions = "Work in {{repo}} on {{branch}} for {{run}}:\n\n{{input}}"
	res := call(t, h, http.MethodPut, "/api/workflows/"+id, wfBody(t, "Changed workflow", d), sameOriginFetch)
	if res.Code != http.StatusOK {
		t.Fatalf("update: %d %s", res.Code, res.Body)
	}
	r = get(t, h, path, nil)
	fixedBody := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(fixedBody, "Custom &lt;script&gt;") || strings.Contains(fixedBody, "<script>alert") || strings.Contains(fixedBody, "Choose by the requested work product") {
		t.Fatalf("updated fixed workflow preview: %d %s", r.Code, r.Body)
	}
	if !strings.Contains(fixedBody, "<p class=\"meta\">codex. Includes the first step.</p>") || strings.Contains(fixedBody, "thinking .") {
		t.Errorf("empty model/thinking rendered dangling separators: %s", fixedBody)
	}
	if strings.Contains(fixedBody, "The project finder uses the host") || !strings.Contains(fixedBody, "filled with the fixed repository <code>/w/app</code>") {
		t.Errorf("fixed workflow note did not describe fixed {{repo}}: %s", fixedBody)
	}
	fixedVars := workflow.Vars{Run: "{{run}}", Input: "{{input}}", Repo: "/w/app", Branch: "{{branch}}"}
	for _, want := range []string{
		workflow.OrchestratorPrompt("Changed workflow", d, fixedVars),
		workflow.StepMessage(d, 0, fixedVars),
	} {
		if !strings.Contains(fixedBody, "<pre>"+html.EscapeString(want)+"</pre>") {
			t.Errorf("missing fixed workflow composed message: %.100s", want)
		}
	}
	for _, p := range []string{"/workflows/not-an-id/prompts", "/workflows/00000000-0000-0000-0000-000000000000/prompts"} {
		if r := get(t, h, p, nil); r.Code != http.StatusNotFound {
			t.Errorf("%s = %d", p, r.Code)
		}
	}
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "prompt-other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if r := call(t, h, http.MethodGet, path, "", nil, sessionFor(t, ts.store, other)); r.Code != http.StatusNotFound {
		t.Errorf("another account = %d", r.Code)
	}
	if r := get(t, anon(h), path, nil); r.Code != http.StatusSeeOther {
		t.Errorf("anonymous = %d", r.Code)
	}
	if r := call(t, h, http.MethodDelete, "/api/workflows/"+id, "", sameOriginFetch); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	if r := get(t, h, path, nil); r.Code != http.StatusNotFound {
		t.Errorf("deleted workflow = %d", r.Code)
	}
}
