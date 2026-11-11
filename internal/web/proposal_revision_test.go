package web

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProposalReplacementAndIdentity(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	headers := map[string]string{"Authorization": "Bearer " + key}
	propose := func(extra string) string {
		r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"create","summary":"Review this","workflow":`+wfBody(t, "New", workflow.Starting)+extra+`}`, headers)
		if r.Code != 201 {
			t.Fatalf("propose: %d %s", r.Code, r.Body)
		}
		var p struct{ ID string }
		json.Unmarshal(r.Body.Bytes(), &p)
		return p.ID
	}
	first := propose(`,"proposedBy":"agent-demo","runId":"run-demo"`)
	r := call(t, anon(h), "POST", "/api/workflow-proposals/"+first+"/withdraw", `{}`, headers)
	if r.Code != 400 {
		t.Fatalf("missing reason: %d", r.Code)
	}
	second := propose(`,"replacesId":"` + first + `"`)
	code, res := sqlPost(t, h, q("SELECT status,withdraw_reason,proposed_by,run_id FROM workflow_proposals WHERE id='"+first+"'"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "withdrawn" || res.Rows[0][1] == "" || res.Rows[0][2] != "agent-demo" || res.Rows[0][3] != "run-demo" {
		t.Fatalf("audit: %d %+v", code, res)
	}
	r = call(t, anon(h), "POST", "/api/workflow-proposals/"+second+"/withdraw", `{"reason":"No longer needed"}`, headers)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	page := get(t, h, "/workflow-proposals/"+second, nil)
	if !strings.Contains(page.Body.String(), "No longer needed") {
		t.Fatal("missing withdrawal reason")
	}
	r = call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"create","summary":"Cannot replace decided","replacesId":"`+second+`","workflow":`+wfBody(t, "New", workflow.Starting)+`}`, headers)
	if r.Code != 409 {
		t.Fatalf("replaced decided: %d %s", r.Code, r.Body)
	}
}

func TestProposalReviewDiffKeepsUnchangedLines(t *testing.T) {
	lines := proposalLineDiff("one\nold\nsame\nlast", "one\nnew\nsame\nlast")
	changed := 0
	for _, line := range lines {
		if line.Kind != "same" {
			changed++
		}
	}
	if changed != 2 {
		t.Fatalf("diff rewrote unchanged lines: %+v", lines)
	}
}

func TestProposalLineDiffReconstructsBothInputs(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for range 300 {
		makeText := func() string {
			lines := []string{}
			for range r.IntN(12) {
				lines = append(lines, []string{"keep", "repeat", "change", ""}[r.IntN(4)])
			}
			return strings.Join(lines, "\n")
		}
		before, after := makeText(), makeText()
		old, new := []string{}, []string{}
		for _, line := range proposalLineDiff(before, after) {
			if line.Kind != "added" {
				old = append(old, line.Text)
			}
			if line.Kind != "removed" {
				new = append(new, line.Text)
			}
		}
		if strings.Join(old, "\n") != before || strings.Join(new, "\n") != after {
			t.Fatalf("diff cannot reconstruct %q → %q", before, after)
		}
	}
	a := &workflow.WorkflowState{Name: "Same", Definition: workflow.Starting, Version: 1}
	b := *a
	b.Version = 2
	if changes := proposalChanges(a, &b); len(changes) != 0 {
		t.Fatalf("version shown as a review change: %+v", changes)
	}
}

func TestProposalReviewReadableAndDiscoverable(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	id := createWorkflow(t, h, workflow.Starting)
	d := workflow.Starting
	d.Steps = append([]workflow.Step(nil), d.Steps...)
	d.Steps[0].Instructions = "Read <id> first.\nKeep this on a new line."
	r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"update","summary":"Readable change","workflowId":"`+id+`","baseVersion":1,"workflow":`+wfBody(t, "Task from Intelligence", d)+`}`, map[string]string{"Authorization": "Bearer " + key})
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &p)
	page := get(t, h, "/workflow-proposals/"+p.ID, nil).Body.String()
	for _, want := range []string{`class="topbar"`, "Changed fields", "Read &lt;id&gt; first.\nKeep this on a new line.", "Raw JSON", "Created", "Expires", "Setting the default"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, `\u003c`) {
		t.Fatal("HTML still JSON-escaped")
	}
	list := get(t, h, "/workflow-proposals", nil).Body.String()
	if !strings.Contains(list, "Task from Intelligence") || !strings.Contains(list, "v1") || !strings.Contains(list, "Created") {
		t.Fatal("list lacks context")
	}
	cards := get(t, h, "/workflows", nil).Body.String()
	if !strings.Contains(cards, "1 pending proposal") {
		t.Fatal("workflow card lacks proposal link")
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/approve", `{}`, sameOriginFetch)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	page = get(t, h, "/workflow-proposals/"+p.ID, nil).Body.String()
	if strings.Contains(page, "Expires ") || !strings.Contains(page, "Decided") || !strings.Contains(page, "/versions/2") {
		t.Fatal("decision context missing")
	}
	version := get(t, h, "/workflows/"+id+"/versions/2", nil)
	if version.Code != 200 || !strings.Contains(version.Body.String(), "Read &lt;id&gt; first.") {
		t.Fatalf("version link: %d", version.Code)
	}
}
