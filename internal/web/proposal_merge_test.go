package web

import (
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/whatsnew"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProposalPagesKeepSharedNavigation(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	proposal := proposalForQuestion(t, h)
	wf := createWorkflow(t, h, workflow.Starting)
	for _, path := range []string{"/workflow-proposals", "/workflow-proposals/" + proposal, "/workflows/" + wf + "/versions/1"} {
		page := get(t, h, path, nil)
		if page.Code != 200 {
			t.Fatalf("%s: %d %s", path, page.Code, page.Body)
		}
		for _, want := range []string{`id="nav-whats-new"`, `src="/whatsnew.js"`, `id="proposals-nav"`, `src="/submitkeys.js"`} {
			if !strings.Contains(page.Body.String(), want) {
				t.Errorf("%s missing %s", path, want)
			}
		}
	}
}

func TestProposalReviewHasReleaseNote(t *testing.T) {
	entries, err := whatsnew.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ID == "workflow-proposal-review" {
			if entry.Media == "" || !strings.Contains(entry.Summary, "Proposals") {
				t.Fatal("proposal release note needs a demo and navigation guidance")
			}
			return
		}
	}
	t.Fatal("proposal review feature is missing from What's New")
}
