package web

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowPromptsPage(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
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
	v := workflow.Vars{Run: "{{run}}", Input: "{{input}}", Repo: "{{repo}}", Branch: "{{branch}}"}
	wants := []string{workflow.ProjectPrompt(v.Run, d, v.Input, []string{"/w/app"}),
		workflow.OrchestratorPrompt("Task from Intelligence", d, v)}
	for i := range d.Steps {
		wants = append(wants, workflow.StepMessage(d, i, v))
	}
	for _, want := range wants {
		if !strings.Contains(r.Body.String(), "<pre>"+html.EscapeString(want)+"</pre>") {
			t.Errorf("missing exact composed message: %.100s", want)
		}
	}
	// Saved edits, rather than the starting definition, must drive the preview.
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	d.Orchestrator.Instructions = "Custom <script>alert('prompt')</script>"
	res := call(t, h, http.MethodPut, "/api/workflows/"+id, wfBody(t, "Changed workflow", d), sameOriginFetch)
	if res.Code != http.StatusOK {
		t.Fatalf("update: %d %s", res.Code, res.Body)
	}
	r = get(t, h, path, nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Custom &lt;script&gt;") || strings.Contains(r.Body.String(), "<script>alert") || strings.Contains(r.Body.String(), "Choose by the requested work product") {
		t.Fatalf("updated fixed workflow preview: %d %s", r.Code, r.Body)
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
