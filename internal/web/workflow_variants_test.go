package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// variantBody is a workflow request for d that names its source (or "" to detach); a nil source
// leaves variantOf out.
func variantBody(t *testing.T, name string, d workflow.Definition, source *string) string {
	t.Helper()
	if d.Host == "" {
		d.Host = "host-1"
	}
	body := map[string]any{"name": name, "description": "for a test", "definition": d}
	if source != nil {
		body["variantOf"] = *source
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// current reads a workflow's version, definition and source through the owner.
func current(t *testing.T, d dbtest.DB, id string) (int, workflow.Definition, string) {
	t.Helper()
	var version int
	var def []byte
	var source string
	if err := d.Owner.QueryRow(t.Context(), `SELECT w.version, v.definition, coalesce(w.variant_of::text, '') FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version WHERE w.id = $1`, id).Scan(&version, &def, &source); err != nil {
		t.Fatal(err)
	}
	var out workflow.Definition
	if err := json.Unmarshal(def, &out); err != nil {
		t.Fatal(err)
	}
	return version, out, source
}

func fastModel(d workflow.Definition) workflow.Definition {
	d.Orchestrator.Provider, d.Orchestrator.Model, d.Orchestrator.Thinking, d.Orchestrator.ServiceTier = "codex", "gpt-6.2-sol", "high", "ultrafast"
	return d
}

func TestAModelVariantKeepsItsSourcesStepsAndPrompt(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	source := createWorkflow(t, h, workflow.Starting)

	// A variant made with a stale copy of the steps takes the source's, and its own model.
	stale := fastModel(workflow.Starting)
	stale.Trigger = workflow.TriggerManual
	stale.Orchestrator.Instructions = "A stale prompt."
	stale.Steps = []workflow.Step{{ID: "old", Kind: workflow.KindStep, Title: "Old", Instructions: "Old step."}}
	rec := call(t, h, http.MethodPost, "/api/workflows", variantBody(t, "Fast", stale, &source), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST variant = %d %s", rec.Code, rec.Body)
	}
	var made struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &made)
	variant := made.ID
	_, def, of := current(t, d, variant)
	if of != source || def.Orchestrator.Model != "gpt-6.2-sol" || def.Orchestrator.ServiceTier != "ultrafast" || def.Trigger != workflow.TriggerManual ||
		def.Orchestrator.Instructions != workflow.Starting.Orchestrator.Instructions || len(def.Steps) != len(workflow.Starting.Steps) || def.Steps[0].ID != workflow.Starting.Steps[0].ID {
		t.Fatalf("variant = of %q %+v", of, def)
	}

	// The source's next version reaches the variant, as a new version of it.
	edited := workflow.Starting
	edited.Orchestrator.Instructions = "The new prompt: finish approved checkouts."
	edited.Steps = append(append([]workflow.Step(nil), workflow.Starting.Steps...), workflow.Step{ID: "captcha", Kind: workflow.KindStep, Title: "Finish", Instructions: "Solve the CAPTCHA."})
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+source, wfBody(t, "Task from Intelligence", edited), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT source = %d %s", rec.Code, rec.Body)
	}
	version, def, _ := current(t, d, variant)
	if version != 2 || def.Orchestrator.Instructions != edited.Orchestrator.Instructions || def.Steps[len(def.Steps)-1].ID != "captcha" ||
		def.Orchestrator.Provider != "codex" || def.Orchestrator.Model != "gpt-6.2-sol" || def.Orchestrator.Thinking != "high" || def.Trigger != workflow.TriggerManual {
		t.Fatalf("after the source changed, variant v%d = %+v", version, def)
	}
	var name string
	d.Owner.QueryRow(t.Context(), "SELECT name FROM workflows WHERE id = $1", variant).Scan(&name)
	if name != "Fast" {
		t.Fatalf("variant renamed to %q", name)
	}

	// Its own model is its to change; the inherited parts are not.
	mine := fastModel(edited)
	mine.Trigger = workflow.TriggerManual
	mine.Orchestrator.ServiceTier = "priority"
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+variant, wfBody(t, "Fast", mine), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT variant model = %d %s", rec.Code, rec.Body)
	}
	if _, def, _ := current(t, d, variant); def.Orchestrator.ServiceTier != "priority" {
		t.Fatalf("variant tier = %q", def.Orchestrator.ServiceTier)
	}
	mine.Steps = mine.Steps[:1]
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+variant, wfBody(t, "Fast", mine), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "variant of") {
		t.Fatalf("PUT variant steps = %d %s", rec.Code, rec.Body)
	}

	// SQL reads which workflow it follows.
	sql := call(t, h, http.MethodGet, "/api/sql?q=SELECT+variant_of::text+FROM+workflows+WHERE+variant_of+IS+NOT+NULL", "", nil)
	if !strings.Contains(sql.Body.String(), source) {
		t.Fatalf("SQL variant_of = %s", sql.Body)
	}

	// The pages say so.
	if page := get(t, h, "/workflows/"+variant, nil); !strings.Contains(page.Body.String(), "Model variant of") {
		t.Fatalf("editor does not say it is a variant")
	}
	if page := get(t, h, "/workflows", nil); !strings.Contains(page.Body.String(), "Variant of") {
		t.Fatalf("list does not say it is a variant")
	}

	// Detached, it keeps what it has and stops following.
	detach := ""
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+variant, variantBody(t, "Fast", mine, &detach), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("detach = %d %s", rec.Code, rec.Body)
	}
	before, def, of := current(t, d, variant)
	if of != "" || len(def.Steps) != 1 {
		t.Fatalf("detached = of %q, %d steps", of, len(def.Steps))
	}
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+source, wfBody(t, "Task from Intelligence", workflow.Starting), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("PUT source = %d %s", rec.Code, rec.Body)
	}
	if after, _, _ := current(t, d, variant); after != before {
		t.Fatalf("a detached copy still follows its source: v%d -> v%d", before, after)
	}
}

func TestAnExistingCopyBecomesAVariantAndKeepsItsTrigger(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	src := workflow.Starting
	src.Trigger = workflow.TriggerEmail
	src.Email = &workflow.EmailFilter{From: []string{"zach@example.com"}}
	src.Orchestrator.Instructions = "An email was received. Finish approved checkouts."
	source := createWorkflow(t, h, src)

	cp := fastModel(src)
	cp.Email = &workflow.EmailFilter{From: []string{"zach@example.com"}, To: "bot+fast@example.com"}
	cp.Orchestrator.Instructions = "An email was received at bot+fast."
	rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Copy", cp), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST copy = %d %s", rec.Code, rec.Body)
	}
	var made struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &made)
	copyID := made.ID
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs(account_id,workflow_id,version,host,title,input,status,created_at,updated_at)
		SELECT account_id,id,1,'host-1','Earlier','Hello','succeeded',now(),now() FROM workflows WHERE id = $1`, copyID); err != nil {
		t.Fatal(err)
	}

	if rec := call(t, h, http.MethodPut, "/api/workflows/"+copyID, variantBody(t, "Copy", cp, &source), sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("convert = %d %s", rec.Code, rec.Body)
	}
	version, def, of := current(t, d, copyID)
	if of != source || version != 2 || def.Orchestrator.Instructions != src.Orchestrator.Instructions || def.Email == nil ||
		def.Email.To != "bot+fast@example.com" || def.Orchestrator.ServiceTier != "ultrafast" {
		t.Fatalf("converted = v%d of %q %+v", version, of, def)
	}
	var runs int
	d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_runs WHERE workflow_id = $1 AND version = 1", copyID).Scan(&runs)
	if runs != 1 {
		t.Fatalf("its run moved: %d runs on v1", runs)
	}
}

func TestAVariantNeedsASourceOfItsOwnAccountThatIsNoVariant(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	ts := h.(*testServer)
	sendReport(t, h, wfReport())
	source := createWorkflow(t, h, workflow.Starting)
	fast := fastModel(workflow.Starting)
	rec := call(t, h, http.MethodPost, "/api/workflows", variantBody(t, "Fast", fast, &source), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST variant = %d %s", rec.Code, rec.Body)
	}
	var made struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &made)

	if rec := call(t, h, http.MethodPost, "/api/workflows", variantBody(t, "Faster", fast, &made.ID), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "itself a variant") {
		t.Errorf("variant of a variant = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+source, variantBody(t, "Self", workflow.Starting, &source), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Errorf("variant of itself = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+source, variantBody(t, "Has one", workflow.Starting, &made.ID), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Errorf("a source with variants becoming a variant = %d %s", rec.Code, rec.Body)
	}
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	joinHost(t, ts.store, bob, "bobs-host")
	asBob := sessionFor(t, ts.store, bob)
	bobs := fast
	bobs.Host = "bobs-host"
	if rec := call(t, h, http.MethodPost, "/api/workflows", variantBody(t, "Bob's", bobs, &source), sameOriginFetch, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("variant of another account's workflow = %d %s", rec.Code, rec.Body)
	}
}

func TestReviewedChangesReachVariantsAndStayOffThem(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	source := createWorkflow(t, h, workflow.Starting)
	rec := call(t, h, http.MethodPost, "/api/workflows", variantBody(t, "Fast", fastModel(workflow.Starting), &source), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST variant = %d %s", rec.Code, rec.Body)
	}
	var made struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &made)
	key := map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")}
	propose := func(id string, version int, d workflow.Definition) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"operation":"update","workflowId":"` + id + `","baseVersion":` + strconv.Itoa(version) + `,"summary":"Change the steps","workflow":` + wfBody(t, "Changed", d) + `}`
		return call(t, anon(h), "POST", "/api/workflow-proposals", body, key)
	}

	edited := workflow.Starting
	edited.Orchestrator.Instructions = "A reviewed prompt."
	if rec := propose(made.ID, 1, fastModel(edited)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "variant of") {
		t.Fatalf("a proposal changing a variant's prompt = %d %s", rec.Code, rec.Body)
	}
	rec = propose(source, 1, edited)
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose source = %d %s", rec.Code, rec.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec := call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/approve", `{}`, sameOriginFetch); rec.Code >= 300 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	if version, def, _ := current(t, d, made.ID); version != 2 || def.Orchestrator.Instructions != "A reviewed prompt." || def.Orchestrator.Model != "gpt-6.2-sol" {
		t.Fatalf("after an approved source change, variant v%d = %+v", version, def.Orchestrator)
	}
}
