package web

import (
	"context"
	"errors"
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

func TestPermissionRestoreStreamIsHostScoped(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	p := hostclient.PermissionResolution{AgentID: agentA1, PermissionID: "p", Status: "stuck", FirstSeenAt: t0, At: t0, NextRetryAt: t0.Add(time.Minute), RetryDelay: time.Minute}
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	got := b.call(hostclient.Frame{Type: hostclient.PermissionRestoreFrame, PermissionRestore: &hostclient.PermissionRestore{}})
	if got.Error != "" || got.PermissionRestore == nil || len(got.PermissionRestore.Records) != 1 {
		t.Fatal(got)
	}
	row := got.PermissionRestore.Records[0]
	if row.Status != "stuck" || !row.NextRetryAt.Equal(p.NextRetryAt) || row.RetryDelay != p.RetryDelay {
		t.Fatal(row)
	}
	other := openStream(t, h, "host-2")
	got = other.call(hostclient.Frame{Type: hostclient.PermissionRestoreFrame, PermissionRestore: &hostclient.PermissionRestore{}})
	if got.Error != "" || got.PermissionRestore == nil || len(got.PermissionRestore.Records) != 0 {
		t.Fatal("another host read audit", got)
	}
	p.Status = "cleared"
	p.At = t0.Add(time.Second)
	p.ClearedAt = p.At
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	got = b.call(hostclient.Frame{Type: hostclient.PermissionRestoreFrame, PermissionRestore: &hostclient.PermissionRestore{}})
	if got.Error != "" || len(got.PermissionRestore.Records) != 0 {
		t.Fatal(got)
	}
}

func TestPermissionRestartClearsPersistedWarning(t *testing.T) {
	h := reported(t)
	b := openStream(t, h, "host-1")
	p := hostclient.PermissionResolution{AgentID: agentA1, PermissionID: "p", Provider: "claude", Mode: "bypassPermissions",
		Status: "stuck", At: t0, FirstSeenAt: t0.Add(-time.Hour), DeniedAt: t0.Add(-time.Hour), StuckAt: t0}
	if ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &p}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// A new host process has an empty cache. Use its actual authenticated restore
	// and update protocol, then reconcile a fresh snapshot with no pending prompts.
	s := hostclient.PermissionSync{Now: func() time.Time { return t0.Add(time.Second) },
		Load: func(context.Context) ([]hostclient.PermissionResolution, error) {
			ack := b.call(hostclient.Frame{Type: hostclient.PermissionRestoreFrame, PermissionRestore: &hostclient.PermissionRestore{}})
			if ack.Error != "" {
				return nil, errors.New(ack.Error)
			}
			return ack.PermissionRestore.Records, nil
		},
		Send: func(_ context.Context, row hostclient.PermissionResolution) error {
			ack := b.call(hostclient.Frame{Type: hostclient.PermissionFrame, Permission: &row})
			if ack.Error != "" {
				return errors.New(ack.Error)
			}
			return nil
		}, Deny: func(context.Context, string, string) error { t.Fatal("denied absent prompt"); return nil }}
	if err := s.Sync(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	code, r := sqlPost(t, h, q("SELECT status, cleared_at IS NOT NULL FROM permission_resolutions"))
	if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != "cleared" || r.Rows[0][1] != true {
		t.Fatal(code, r)
	}
	for _, path := range []string{"/", "/hosts/host-1/agents/" + agentA1} {
		if body := get(t, h, path, nil).Body.String(); strings.Contains(body, "permission recovery stuck") {
			t.Fatal("stale restart warning", path)
		}
	}
}
