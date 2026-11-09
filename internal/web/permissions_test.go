package web

import (
	"github.com/zachlatta/intelligence/internal/hostclient"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamPermissionAuditIsReadableThroughSQL(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	p := hostclient.PermissionResolution{AgentID: agentA1, PermissionID: "permission-1", Provider: "claude", Mode: "bypassPermissions", Tool: "Bash", Status: "denied", Message: "rewrite", At: t0}
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	code, r := sqlPost(t, h, q("SELECT status, tool FROM permission_resolutions"))
	if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != "denied" || r.Rows[0][1] != "Bash" {
		t.Fatal(code, r)
	}
	p.Status = "bogus"
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error == "" {
		t.Fatal("accepted invalid status")
	}
}

func TestStuckPermissionAppearsOnHostAndAgentPages(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	p := hostclient.PermissionResolution{AgentID: agentA1, PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Tool: "Bash", Status: "stuck", Message: "rewrite", At: t0, FirstSeenAt: t0.Add(-20 * time.Minute), StuckAt: t0}
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	for _, path := range []string{"/", "/hosts/host-1/agents/" + agentA1} {
		if body := get(t, h, path, nil).Body.String(); !strings.Contains(body, "permission recovery stuck") {
			t.Fatal(path, "missing stuck indicator")
		}
	}
	p.Status = "cleared"
	p.At = t0.Add(time.Second)
	p.ClearedAt = p.At
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if body := get(t, h, "/hosts/host-1/agents/"+agentA1, nil).Body.String(); strings.Contains(body, "permission recovery stuck") {
		t.Fatal("stale stuck indicator")
	}
}
