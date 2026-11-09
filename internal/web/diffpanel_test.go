package web

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
)

func sampleDiff() hostclient.WorkspaceDiff {
	return hostclient.WorkspaceDiff{WorkspaceID: "w-app", Directory: "/w/app-login", CollectedAt: t0, Additions: 1, Deletions: 1,
		Files: []paseo.DiffFile{{Path: "login.go", Additions: 1, Deletions: 1, Status: "ok", Hunks: []paseo.DiffHunk{{OldStart: 3,
			OldCount: 1, NewStart: 3, NewCount: 1, Lines: []paseo.DiffLine{{Type: "header", Content: "@@ -3 +3 @@"},
				{Type: "remove", Content: "old <b>"}, {Type: "add", Content: "new"}}}}}}}
}

func TestStreamStoresWorkspaceDiffs(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	d := sampleDiff()
	if ack := b.call(hostclient.Frame{Type: hostclient.DiffFrame, Diff: &d}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if ack := b.call(hostclient.Frame{Type: hostclient.DiffFrame, Diff: &hostclient.WorkspaceDiff{}}); ack.Error == "" {
		t.Error("a diff without a workspace was accepted")
	}
	code, r := sqlPost(t, h, q("SELECT path, additions, deletions FROM workspace_diff_files WHERE workspace_id = 'w-app'"))
	if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != "login.go" {
		t.Errorf("status %d, rows %v, %s", code, r.Rows, r.Error)
	}
}

// The agent page has a sidebar of the agents in its workspace and a diff panel, each folding away.
func TestTranscriptPageHasTheWorkspacesAgentsAndItsDiff(t *testing.T) {
	h := reported(t)
	page := get(t, h, "/hosts/host-1/agents/"+agentA1, nil).Body.String()

	side := between(t, page, `<aside id="side"`, `</aside>`)
	for _, want := range []string{`href="/hosts/host-1/agents/` + agentA1 + `" aria-current="page"`,
		`href="/hosts/host-1/agents/a3000000-0000-4000-8000-000000000003"`, "Audit", "Login fix"} {
		if !strings.Contains(side, want) {
			t.Errorf("sidebar misses %q", want)
		}
	}
	if strings.Contains(side, "Write notes") {
		t.Error("the sidebar lists an agent of another workspace")
	}
	for _, want := range []string{`data-panel="side"`, `data-panel="diff"`, `aria-controls="side"`, `aria-controls="diff"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page misses the toggle %q", want)
		}
	}

	panel := between(t, page, `<aside id="diff"`, `</aside>`)
	m := regexp.MustCompile(`data-diff-sql="([^"]*)"`).FindStringSubmatch(panel)
	if m == nil {
		t.Fatalf("the diff panel has no query: %s", panel)
	}
	query := html.UnescapeString(m[1])

	// before the host sends the diff: the query finds nothing
	if code, r := sqlPost(t, h, q(query)); code != http.StatusOK || len(r.Rows) != 0 {
		t.Fatalf("before a diff: status %d, rows %v, %s", code, r.Rows, r.Error)
	}
	b := openStream(t, h, "host-1")
	d := sampleDiff()
	if ack := b.call(hostclient.Frame{Type: hostclient.DiffFrame, Diff: &d}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	code, r := sqlPost(t, h, q(query))
	if code != http.StatusOK || len(r.Rows) != 1 {
		t.Fatalf("status %d, rows %v, %s", code, r.Rows, r.Error)
	}
	cols := map[string]any{}
	for i, c := range r.Columns {
		cols[c.Name] = r.Rows[0][i]
	}
	files, _ := cols["files"].([]any)
	if cols["additions"] != float64(1) || cols["error"] != "" || len(files) != 1 || cols["collected_at"] == nil {
		t.Errorf("diff row = %v", cols)
	}
	if f, _ := files[0].(map[string]any); f["path"] != "login.go" {
		t.Errorf("file = %v", files[0])
	}
}

// A directory outside git has no diff panel to open.
func TestTranscriptPageOfAPlainDirectoryHasNoDiff(t *testing.T) {
	h := reported(t)
	page := get(t, h, "/hosts/host-1/agents/a2000000-0000-4000-8000-000000000002", nil).Body.String()
	if strings.Contains(page, `<aside id="diff"`) || strings.Contains(page, `data-panel="diff"`) {
		t.Error("a plain directory's agent page has a diff panel")
	}
	if !strings.Contains(page, `<aside id="side"`) {
		t.Error("the sidebar is missing")
	}
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("no %q in the page", start)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return s[i : i+j]
}
