package fleet

import (
	"github.com/zachlatta/intelligence/internal/hostclient"
	"testing"
)

func TestPermissionAuditSurvivesReportsAndRetries(t *testing.T) {
	r := New(dbtestOwner(t))
	record(t, r, full(), t1)
	p := hostclient.PermissionResolution{AgentID: "a1", PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Tool: "Bash", Status: "pending", Message: "rewrite", At: t1}
	if err := r.RecordPermission(ctx, "host-1", p, t1); err != nil {
		t.Fatal(err)
	}
	p.Status = "denied"
	for range 2 {
		if err := r.RecordPermission(ctx, "host-1", p, t1); err != nil {
			t.Fatal(err)
		}
	}
	p.Status = "pending"
	if err := r.RecordPermission(ctx, "host-1", p, t1); err != nil {
		t.Fatal(err)
	}
	record(t, r, full(), t1)
	var n int
	if err := r.p.QueryRow(ctx, "SELECT count(*) FROM permission_resolutions WHERE host='host-1' AND status='denied'").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
