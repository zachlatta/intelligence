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
	wsOps := rep.Paseo.Workspaces[0]
	wsOps.ID, wsOps.Project, wsOps.ProjectRoot = "ws-ops", "demo-contracts", "/w/demo-contracts"
	wsAlpha := rep.Paseo.Workspaces[0]
	wsAlpha.ID, wsAlpha.Project, wsAlpha.ProjectRoot = "ws-alpha", "alpha", "/w/alpha"
	wsBeta := rep.Paseo.Workspaces[0]
	wsBeta.ID, wsBeta.Project, wsBeta.ProjectRoot = "ws-beta", "beta", "/w/beta"
	rep.Paseo.Workspaces = []hostclient.Workspace{wsOps, wsAlpha, wsBeta}
	if err := reg.Record(ctx, rep, time.Now()); err != nil {
		t.Fatal(err)
	}

	const (
		commitMain    = "1111111111111111111111111111111111111111"
		commitOffMain = "2222222222222222222222222222222222222222"
		cRoot         = "10000000-0000-4000-8000-000000000001"
		cAlpha        = "10000000-0000-4000-8000-000000000010"
		cAlphaLeaf    = "10000000-0000-4000-8000-000000000011"
		cBetaOverride = "10000000-0000-4000-8000-000000000015"
		cBetaOvLeaf   = "10000000-0000-4000-8000-000000000016"
		cBeta         = "10000000-0000-4000-8000-000000000020"
		cBetaLeaf     = "10000000-0000-4000-8000-000000000021"
		cUnmapRoot    = "10000000-0000-4000-8000-000000000090"
		cUnmapLeaf    = "10000000-0000-4000-8000-000000000091"
		cUnrepRoot    = "10000000-0000-4000-8000-000000000099"
	)
	const gitOutline = `- Contracts <!-- id:10000000-0000-4000-8000-000000000001 -->
  - Alpha <!-- id:10000000-0000-4000-8000-000000000010 -->
    - Alpha leaf <!-- id:10000000-0000-4000-8000-000000000011 -->
    - Beta override <!-- id:10000000-0000-4000-8000-000000000015 -->
      - Beta override leaf <!-- id:10000000-0000-4000-8000-000000000016 -->
  - Beta <!-- id:10000000-0000-4000-8000-000000000020 -->
    - Beta leaf <!-- id:10000000-0000-4000-8000-000000000021 -->
  - Unmapped <!-- id:10000000-0000-4000-8000-000000000090 -->
    - Unmapped leaf <!-- id:10000000-0000-4000-8000-000000000091 -->
  - Unreported <!-- id:10000000-0000-4000-8000-000000000099 -->
`

	// Background responder simulating the host fulfilling read_git_file commands via real Postgres commands table.
	stopHost := make(chan struct{})
	defer close(stopHost)
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopHost:
				return
			case <-tick.C:
				rows, err := d.Owner.Query(ctx, `SELECT id::text, args FROM commands WHERE host = 'host-1' AND kind = $1 AND status = 'queued'`, hostclient.ReadGitFileKind)
				if err != nil {
					continue
				}
				type item struct {
					id   string
					args []byte
				}
				var items []item
				for rows.Next() {
					var it item
					if rows.Scan(&it.id, &it.args) == nil {
						items = append(items, it)
					}
				}
				rows.Close()
				for _, it := range items {
					var a hostclient.ReadGitFileArgs
					_ = json.Unmarshal(it.args, &a)
					if a.Directory == "/w/demo-contracts" && a.Path == "outliner/outline.md" && a.Commit == commitMain {
						res, _ := json.Marshal(hostclient.CommandResult{Content: gitOutline})
						_ = reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: it.id, Status: hostclient.CommandSucceeded, Result: res}, time.Now())
					} else {
						_ = reg.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: it.id, Status: hostclient.CommandFailed, Error: "commit " + a.Commit + " is not on origin/main"}, time.Now())
					}
				}
			}
		}
	}()

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

	// Valid mapped workflow with Git treeSource verification.
	def := workflow.Definition{
		Host:       "host-1",
		Trigger:    workflow.TriggerManual,
		APITrigger: true,
		Project: workflow.Project{
			Mode: workflow.ProjectMap,
			Routes: []workflow.ProjectRoute{
				{Slug: "org/alpha", Directory: "/w/alpha", Keys: []string{cAlpha}},
				{Slug: "org/beta", Directory: "/w/beta", Keys: []string{cBetaOverride, cBeta}},
				{Slug: "org/unreported", Directory: "/w/unreported", Keys: []string{cUnrepRoot}},
			},
			TreeSource: &workflow.ProjectTreeSource{
				Directory: "/w/demo-contracts",
				Path:      "outliner/outline.md",
			},
		},
		Orchestrator: workflow.Orchestrator{Provider: "claude", Model: "opus", Instructions: "Orchestrate mapped fix."},
		Steps:        []workflow.Step{{ID: "work", Kind: workflow.KindStep, Title: "Work", Instructions: "Fix in {{repo}}."}},
	}
	wfID := createWorkflow(t, h, def)
	_, apiKey := issueAPIKey(t, h, wfID, "mapped-client")

	// Editor HTML exposes deterministic map mode + Git tree verification UI.
	editorRec := call(t, h, http.MethodGet, "/workflows/"+wfID, "", nil)
	if editorRec.Code != http.StatusOK || !strings.Contains(editorRec.Body.String(), "Git tree verification (optional)") {
		t.Fatalf("GET workflow editor = %d (missing Git tree verification UI)", editorRec.Code)
	}

	countRuns := func() int {
		var n int
		if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wfID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

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

	// 1. Valid Git-verified contract_tree -> 201, repo=/w/alpha, project_selected=true, create_workspace (no finder).
	rAlpha := startMapped("---\ncontract_ids:\n  - " + cAlphaLeaf + "\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cAlphaLeaf + "\n---\nFix alpha leaf.\n")
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

	// 2. Child override via real Git tree -> /w/beta.
	rBeta := startMapped("---\ncontract_ids: [" + cBetaOvLeaf + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cBetaOverride + "\n            children:\n              - id: " + cBetaOvLeaf + "\n---\nFix beta override.\n")
	if rBeta.Repo != "/w/beta" {
		t.Fatalf("child override repo = %q, want /w/beta", rBeta.Repo)
	}

	// 3. Same-repo multi-contract batch -> /w/beta.
	rBatch := startMapped("---\ncontract_ids:\n  - " + cBetaOvLeaf + "\n  - " + cBetaLeaf + "\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cBetaOverride + "\n            children:\n              - id: " + cBetaOvLeaf + "\n      - id: " + cBeta + "\n        children:\n          - id: " + cBetaLeaf + "\n---\nBatch beta.\n")
	if rBatch.Repo != "/w/beta" {
		t.Fatalf("same-repo batch repo = %q, want /w/beta", rBatch.Repo)
	}

	beforeRefusals := countRuns()
	if beforeRefusals != 3 {
		t.Fatalf("expected 3 runs before refusal probes, got %d", beforeRefusals)
	}

	// 4. All spoofed/invalid/unmapped/mixed inputs MUST fail closed (400) with ZERO new runs created.
	refusalCases := map[string]struct {
		input string
		want  string
	}{
		"P1 legacy contract_ancestors": {
			"---\ncontract_ids: [" + cUnmapLeaf + "]\ncontract_ancestors:\n  " + cUnmapLeaf + ": [" + cUnmapLeaf + ", " + cAlpha + "]\n---\nBody",
			"unverified ancestor chains",
		},
		"P1 spoofed contract_tree (unmapped placed under alpha)": {
			"---\ncontract_ids: [" + cUnmapLeaf + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cUnmapLeaf + "\n---\nBody",
			"does not match Git outline",
		},
		"P2 spoofed contract_tree (alpha placed under beta)": {
			"---\ncontract_ids: [" + cAlphaLeaf + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cBeta + "\n        children:\n          - id: " + cAlphaLeaf + "\n---\nBody",
			"does not match Git outline",
		},
		"P3 partial subtree missing real root": {
			"---\ncontract_ids: [" + cAlphaLeaf + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cAlpha + "\n    children:\n      - id: " + cAlphaLeaf + "\n---\nBody",
			"does not match Git outline",
		},
		"P4 quoted target_repo": {
			"---\ncontract_ids: [" + cAlphaLeaf + "]\n\"target_repo\": /w/beta\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n---\nBody",
			"must not specify repository field",
		},
		"off-main commit refused by host read_git_file": {
			"---\ncontract_ids: [" + cAlphaLeaf + "]\noutline_commit_sha: " + commitOffMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cAlphaLeaf + "\n---\nBody",
			"not on origin/main",
		},
		"unmapped contract with real Git tree": {
			"---\ncontract_ids: [" + cUnmapLeaf + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cUnmapRoot + "\n        children:\n          - id: " + cUnmapLeaf + "\n---\nBody",
			"no project route matches",
		},
		"mixed-repo batch with real Git tree": {
			"---\ncontract_ids:\n  - " + cAlphaLeaf + "\n  - " + cBetaLeaf + "\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cAlpha + "\n        children:\n          - id: " + cAlphaLeaf + "\n      - id: " + cBeta + "\n        children:\n          - id: " + cBetaLeaf + "\n---\nBody",
			"span multiple repositories",
		},
		"unreported host repository": {
			"---\ncontract_ids: [" + cUnrepRoot + "]\noutline_commit_sha: " + commitMain + "\ncontract_tree:\n  - id: " + cRoot + "\n    children:\n      - id: " + cUnrepRoot + "\n---\nBody",
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

	// 5. Proposal diff captures Project routes and Project tree source changes cleanly.
	prev := &workflow.WorkflowState{Name: "Fix", Definition: apiWorkflowDef("host-1")}
	next := &workflow.WorkflowState{Name: "Fix", Definition: def}
	changes := proposalChanges(prev, next)
	sawRoutes, sawTreeSrc := false, false
	for _, ch := range changes {
		if ch.Label == "Project routes" && strings.Contains(ch.After, "/w/alpha") {
			sawRoutes = true
		}
		if ch.Label == "Project tree source" && strings.Contains(ch.After, "/w/demo-contracts") {
			sawTreeSrc = true
		}
	}
	if !sawRoutes || !sawTreeSrc {
		t.Fatalf("proposalChanges missing Project routes/treeSource diffs: sawRoutes=%v sawTreeSrc=%v", sawRoutes, sawTreeSrc)
	}
}
