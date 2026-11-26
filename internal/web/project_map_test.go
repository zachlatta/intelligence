package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProjectMapRoutingEnforcedBeforeRunCreation(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "test")
	reg := fleet.New(d.Owner)
	ctx := context.Background()
	_ = keyOf(t, h, "host-1")

	rep := wfReport()
	wsAlpha := rep.Paseo.Workspaces[0]
	wsAlpha.ID, wsAlpha.Project, wsAlpha.ProjectRoot = "ws-alpha", "alpha", "/w/alpha"
	wsBeta := rep.Paseo.Workspaces[0]
	wsBeta.ID, wsBeta.Project, wsBeta.ProjectRoot = "ws-beta", "beta", "/w/beta"
	rep.Paseo.Workspaces = []hostclient.Workspace{wsAlpha, wsBeta}
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Invalid mapped definitions are refused at save time (400).
	badDef := workflow.Definition{
		Host:         "host-1",
		Trigger:      workflow.TriggerManual,
		APITrigger:   true,
		Project:      workflow.Project{Mode: workflow.ProjectMap},
		Orchestrator: workflow.Orchestrator{Provider: "claude", Model: "opus", Instructions: "Orchestrate."},
		Steps:        []workflow.Step{{ID: "work", Kind: workflow.KindStep, Title: "Work", Instructions: "Work."}},
	}
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Empty Map", badDef), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty routes map = %d %s, want 400", rec.Code, rec.Body)
	}

	// Valid mapped workflow with two reported repos and one unreported repo.
	def := workflow.Definition{
		Host:       "host-1",
		Trigger:    workflow.TriggerManual,
		APITrigger: true,
		Project: workflow.Project{
			Mode: workflow.ProjectMap,
			Routes: []workflow.ProjectRoute{
				{Slug: "org/alpha", Directory: "/w/alpha", Keys: []string{"root-alpha"}},
				{Slug: "org/beta", Directory: "/w/beta", Keys: []string{"child-beta-override", "root-beta"}},
				{Slug: "org/unreported", Directory: "/w/unreported", Keys: []string{"root-unreported"}},
			},
		},
		Orchestrator: workflow.Orchestrator{Provider: "claude", Model: "opus", Instructions: "Orchestrate mapped fix."},
		Steps:        []workflow.Step{{ID: "work", Kind: workflow.KindStep, Title: "Work", Instructions: "Fix in {{repo}}."}},
	}
	wfID := createWorkflow(t, h, def)
	_, apiKey := issueAPIKey(t, h, wfID, "mapped-client")

	// Editor HTML exposes the deterministic map mode UI.
	editorRec := call(t, h, http.MethodGet, "/workflows/"+wfID, "", nil)
	if editorRec.Code != http.StatusOK || !strings.Contains(editorRec.Body.String(), "Mapped from input keys (deterministic)") {
		t.Fatalf("GET workflow editor = %d (missing map mode option)", editorRec.Code)
	}

	countRuns := func() int {
		var n int
		if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wfID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 1. Inherited ancestor match -> 201, repo=/w/alpha, project_selected=true, immediate create_workspace (no finder).
	startMapped := func(input string) apiRunSummary {
		body, _ := json.Marshal(map[string]string{"input": input})
		rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", apiKey, string(body))
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST start run = %d %s, want 201", rec.Code, rec.Body)
		}
		var out apiRunSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	rAlpha := startMapped("---\ncontract_ids:\n  - leaf-a1\ncontract_ancestors:\n  leaf-a1:\n    - leaf-a1\n    - root-alpha\n---\nFix alpha leaf.\n")
	if rAlpha.Repo != "/w/alpha" {
		t.Fatalf("alpha run repo = %q, want /w/alpha", rAlpha.Repo)
	}
	var projSelected bool
	var cmdKind string
	if err := d.Owner.QueryRow(ctx, `SELECT r.project_selected, c.kind FROM workflow_runs r JOIN commands c ON c.id = r.pending_command WHERE r.id = $1`, rAlpha.ID).Scan(&projSelected, &cmdKind); err != nil {
		t.Fatal(err)
	}
	if !projSelected || cmdKind != hostclient.CreateWorkspaceKind {
		t.Fatalf("mapped run project_selected=%v cmdKind=%q, want true + %q", projSelected, cmdKind, hostclient.CreateWorkspaceKind)
	}

	// 2. Child override beats parent route -> /w/beta.
	rBeta := startMapped("---\ncontract_ids:\n  - leaf-under-override\ncontract_ancestors:\n  leaf-under-override: [leaf-under-override, child-beta-override, root-alpha]\n---\nFix beta override.\n")
	if rBeta.Repo != "/w/beta" {
		t.Fatalf("child override repo = %q, want /w/beta", rBeta.Repo)
	}

	// 3. Same-repo multi-contract batch succeeds -> /w/beta.
	rBatch := startMapped("---\ncontract_ids:\n  - leaf-b1\n  - leaf-b2\ncontract_ancestors:\n  leaf-b1: [leaf-b1, child-beta-override, root-alpha]\n  leaf-b2: [leaf-b2, root-beta]\n---\nBatch beta.\n")
	if rBatch.Repo != "/w/beta" {
		t.Fatalf("same-repo batch repo = %q, want /w/beta", rBatch.Repo)
	}

	beforeRefusals := countRuns()
	if beforeRefusals != 3 {
		t.Fatalf("expected 3 runs before refusal probes, got %d", beforeRefusals)
	}

	// 4. All invalid / unsafe inputs must fail closed (400) with ZERO new runs created.
	refusalCases := map[string]struct {
		input string
		want  string
	}{
		"unmapped contract": {
			"---\ncontract_ids:\n  - unknown-leaf\ncontract_ancestors:\n  unknown-leaf: [unknown-leaf, unknown-root]\n---\nBody",
			"no project route matches",
		},
		"mixed-repo batch": {
			"---\ncontract_ids:\n  - leaf-a\n  - leaf-b\ncontract_ancestors:\n  leaf-a: [leaf-a, root-alpha]\n  leaf-b: [leaf-b, root-beta]\n---\nBody",
			"span multiple repositories",
		},
		"caller-injected target_repo": {
			"---\ncontract_ids: [root-alpha]\ntarget_repo: /w/beta\n---\nBody",
			"must not specify repository field",
		},
		"spoofed ancestor chain": {
			"---\ncontract_ids: [unmapped-id]\ncontract_ancestors:\n  unmapped-id: [root-alpha]\n---\nBody",
			"must begin with",
		},
		"unreported host repository": {
			"---\ncontract_ids: [root-unreported]\n---\nBody",
			"not reported on host",
		},
	}
	for name, tc := range refusalCases {
		body, _ := json.Marshal(map[string]string{"input": tc.input})
		rec := callAPI(t, h, http.MethodPost, "/api/workflow-api/runs", apiKey, string(body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: got %d %s, want 400 containing %q", name, rec.Code, rec.Body, tc.want)
		}
		if got := countRuns(); got != beforeRefusals {
			t.Fatalf("%s: created a run on refusal (count=%d, want %d)", name, got, beforeRefusals)
		}
	}

	// 5. Proposal diff captures Project routes changes cleanly.
	prev := &workflow.WorkflowState{Name: "Fix", Definition: apiWorkflowDef("host-1")}
	next := &workflow.WorkflowState{Name: "Fix", Definition: def}
	changes := proposalChanges(prev, next)
	sawRoutes := false
	for _, ch := range changes {
		if ch.Label == "Project routes" && strings.Contains(ch.After, "/w/alpha") {
			sawRoutes = true
		}
	}
	if !sawRoutes {
		t.Fatalf("proposalChanges did not include Project routes diff: %+v", changes)
	}
}
