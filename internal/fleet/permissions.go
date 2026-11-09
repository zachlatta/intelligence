package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func (r *Registry) RecordPermission(ctx context.Context, host string, p hostclient.PermissionResolution, at time.Time) error {
	if p.AgentID == "" || p.PermissionID == "" || p.At.IsZero() || p.RetryDelay < 0 ||
		(p.Status != "pending" && p.Status != "denied" && p.Status != "error" && p.Status != "stuck" && p.Status != "cleared") {
		return errors.New("invalid permission audit")
	}
	if p.FirstSeenAt.IsZero() {
		p.FirstSeenAt = p.At
	}
	_, err := r.p.Exec(ctx, `INSERT INTO permission_resolutions
 (host,agent_id,permission_id,provider,mode,tool,status,message,error,attempted_at,received_at,first_seen_at,denied_at,stuck_at,cleared_at,next_retry_at,retry_delay_ns)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
 ON CONFLICT(host,agent_id,permission_id) DO UPDATE SET
 status=EXCLUDED.status,error=EXCLUDED.error,attempted_at=EXCLUDED.attempted_at,received_at=EXCLUDED.received_at,
 denied_at=coalesce(EXCLUDED.denied_at,permission_resolutions.denied_at),
 stuck_at=coalesce(EXCLUDED.stuck_at,permission_resolutions.stuck_at),
 cleared_at=coalesce(EXCLUDED.cleared_at,permission_resolutions.cleared_at),
 next_retry_at=EXCLUDED.next_retry_at,retry_delay_ns=EXCLUDED.retry_delay_ns
 WHERE permission_resolutions.attempted_at <= EXCLUDED.attempted_at AND
 CASE permission_resolutions.status
 WHEN 'cleared' THEN false
 WHEN 'stuck' THEN EXCLUDED.status IN ('stuck','cleared')
 WHEN 'denied' THEN EXCLUDED.status IN ('denied','stuck','cleared')
 ELSE true END`,
		host, p.AgentID, p.PermissionID, p.Provider, p.Mode, p.Tool, p.Status, p.Message, p.Error, p.At, at, p.FirstSeenAt, nullTime(p.DeniedAt), nullTime(p.StuckAt), nullTime(p.ClearedAt), nullTime(p.NextRetryAt), int64(p.RetryDelay))
	return err
}

// PermissionRecords returns only this authenticated host's unresolved audits,
// with a stable key cursor so reconnects do not require an unbounded frame.
func (r *Registry) PermissionRecords(ctx context.Context, host, afterAgent, afterPermission string) ([]hostclient.PermissionResolution, error) {
	rows, err := r.p.Query(ctx, `SELECT agent_id,permission_id,provider,mode,tool,status,message,error,
 first_seen_at,denied_at,stuck_at,cleared_at,attempted_at,next_retry_at,retry_delay_ns
 FROM permission_resolutions WHERE host=$1 AND status<>'cleared' AND cleared_at IS NULL
 AND (agent_id,permission_id)>($2,$3)
 ORDER BY agent_id,permission_id LIMIT $4`, host, afterAgent, afterPermission, hostclient.PermissionRestorePage)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (hostclient.PermissionResolution, error) {
		var p hostclient.PermissionResolution
		var denied, stuck, cleared, next *time.Time
		var delay int64
		err := row.Scan(&p.AgentID, &p.PermissionID, &p.Provider, &p.Mode, &p.Tool, &p.Status, &p.Message, &p.Error,
			&p.FirstSeenAt, &denied, &stuck, &cleared, &p.At, &next, &delay)
		p.DeniedAt, p.StuckAt, p.ClearedAt, p.NextRetryAt = deref(denied), deref(stuck), deref(cleared), deref(next)
		p.RetryDelay = time.Duration(delay)
		return p, err
	})
}
