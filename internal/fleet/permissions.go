package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func (r *Registry) RecordPermission(ctx context.Context, host string, p hostclient.PermissionResolution, at time.Time) error {
	if p.AgentID == "" || p.PermissionID == "" || p.At.IsZero() ||
		(p.Status != "pending" && p.Status != "denied" && p.Status != "error" && p.Status != "stuck" && p.Status != "cleared") {
		return errors.New("invalid permission audit")
	}
	if p.FirstSeenAt.IsZero() {
		p.FirstSeenAt = p.At
	}
	_, err := r.p.Exec(ctx, `INSERT INTO permission_resolutions
 (host,agent_id,permission_id,provider,mode,tool,status,message,error,attempted_at,received_at,first_seen_at,denied_at,stuck_at,cleared_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
 ON CONFLICT(host,agent_id,permission_id) DO UPDATE SET
 status=EXCLUDED.status,error=EXCLUDED.error,attempted_at=EXCLUDED.attempted_at,received_at=EXCLUDED.received_at,
 denied_at=coalesce(EXCLUDED.denied_at,permission_resolutions.denied_at),
 stuck_at=coalesce(EXCLUDED.stuck_at,permission_resolutions.stuck_at),
 cleared_at=coalesce(EXCLUDED.cleared_at,permission_resolutions.cleared_at)
 WHERE permission_resolutions.attempted_at <= EXCLUDED.attempted_at AND
 CASE permission_resolutions.status
 WHEN 'cleared' THEN false
 WHEN 'stuck' THEN EXCLUDED.status IN ('stuck','cleared')
 WHEN 'denied' THEN EXCLUDED.status IN ('denied','stuck','cleared')
 ELSE true END`,
		host, p.AgentID, p.PermissionID, p.Provider, p.Mode, p.Tool, p.Status, p.Message, p.Error, p.At, at, p.FirstSeenAt, nullTime(p.DeniedAt), nullTime(p.StuckAt), nullTime(p.ClearedAt))
	return err
}
