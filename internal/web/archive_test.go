package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestHomePageArchivesWorkspacesAndAgentsThroughTheCommandsAPI(t *testing.T) {
	h := reported(t)
	page := get(t, h, "/", nil).Body.String()
	for _, want := range []string{
		// w-app has two agents, one running, and uncommitted changes in its worktree
		`data-archive="workspace" data-host="host-1" data-id="w-app" data-name="Login fix" data-agents="2" data-running="1" data-dirty="1"`,
		`data-archive="workspace" data-host="host-1" data-id="w-docs" data-name="Docs" data-agents="1" data-running="0" data-dirty=""`,
		`data-archive="workspace" data-host="host-1" data-id="w-idle" data-name="Quiet checkout" data-agents="0" data-running="0"`,
		`data-archive="agent" data-host="host-1" data-id="` + agentA1 + `" data-name="Fix the &lt;b&gt;login&lt;/b&gt; page" data-running="1"`,
		`data-archive="agent" data-host="host-1" data-id="a2000000-0000-4000-8000-000000000002" data-name="Write notes" data-running="0"`,
		`function archive(`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("home page misses %q", want)
		}
	}
	for _, body := range []string{`{"kind":"archive_workspace","args":{"workspaceId":"w-app"}}`, `{"kind":"archive_agent","args":{"agentId":"` + agentA1 + `"}}`} {
		if rec := call(t, h, http.MethodPost, "/api/hosts/host-1/commands", body, sameOriginFetch); rec.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestAgentPageHasAnArchiveButton(t *testing.T) {
	h := reported(t)
	page := get(t, h, "/hosts/host-1/agents/"+agentA1, nil).Body.String()
	for _, want := range []string{`data-archive="agent" data-host="host-1" data-id="` + agentA1 + `"`, `data-running="1"`, `data-then="/"`, `function archive(`} {
		if !strings.Contains(page, want) {
			t.Errorf("agent page misses %q", want)
		}
	}
}
