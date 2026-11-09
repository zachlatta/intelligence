package fleet

import (
	"github.com/zachlatta/intelligence/internal/hostclient"
	"testing"
	"time"
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

func TestPermissionAuditEscalatesAndClearsOneRow(t *testing.T) {
	r := New(dbtestOwner(t))
	record(t, r, full(), t1)
	p := hostclient.PermissionResolution{AgentID: "a1", PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Tool: "Bash", Status: "denied", Message: "rewrite", At: t1, FirstSeenAt: t1, DeniedAt: t1}
	if err := r.RecordPermission(ctx, "host-1", p, t1); err != nil {
		t.Fatal(err)
	}
	p.Status = "stuck"
	p.At = t1.Add(20 * time.Minute)
	p.StuckAt = p.At
	if err := r.RecordPermission(ctx, "host-1", p, p.At); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.p.QueryRow(ctx, "SELECT count(*) FROM permission_resolutions WHERE status='stuck' AND stuck_at IS NOT NULL AND denied_at IS NOT NULL AND first_seen_at=$1", t1).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	p.Status = "cleared"
	p.At = p.At.Add(time.Second)
	p.ClearedAt = p.At
	if err := r.RecordPermission(ctx, "host-1", p, p.At); err != nil {
		t.Fatal(err)
	}
	p.Status = "stuck"
	if err := r.RecordPermission(ctx, "host-1", p, p.At); err != nil {
		t.Fatal(err)
	}
	if err := r.p.QueryRow(ctx, "SELECT count(*) FROM permission_resolutions WHERE status='cleared' AND cleared_at IS NOT NULL").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
