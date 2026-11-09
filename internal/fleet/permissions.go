package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func (r *Registry) RecordPermission(ctx context.Context, host string, p hostclient.PermissionResolution, at time.Time) error {
	if p.AgentID == "" || p.PermissionID == "" || p.At.IsZero() ||
		(p.Status != "pending" && p.Status != "denied" && p.Status != "error") {
		return errors.New("invalid permission audit")
	}
	_, err := r.p.Exec(ctx, `INSERT INTO permission_resolutions
 (host,agent_id,permission_id,provider,mode,tool,status,message,error,attempted_at,received_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
 ON CONFLICT(host,agent_id,permission_id) DO UPDATE SET
 status=EXCLUDED.status,error=EXCLUDED.error,attempted_at=EXCLUDED.attempted_at,received_at=EXCLUDED.received_at
 WHERE permission_resolutions.status <> 'denied' AND permission_resolutions.attempted_at <= EXCLUDED.attempted_at`,
		host, p.AgentID, p.PermissionID, p.Provider, p.Mode, p.Tool, p.Status, p.Message, p.Error, p.At, at)
	return err
}
