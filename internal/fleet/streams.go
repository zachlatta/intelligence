package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attach makes stream (a uuid) host's current stream, seen at at, and announces it, so the stream
// it had before ends on whichever server instance holds it. A host that has never reported gets
// its row here.
func (r *Registry) Attach(ctx context.Context, host, stream string, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO hosts (name, last_seen, stream_id) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET last_seen = EXCLUDED.last_seen, stream_id = EXCLUDED.stream_id, start_retry_keys = false`,
			host, at, stream); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, notifySQL, StreamTopic(host))
		return err
	})
}

// Seen records that stream was heard from at at, and reports whether it is still host's stream
// (false: another stream has attached since, and this one should end).
func (r *Registry) Seen(ctx context.Context, host, stream string, at time.Time) (bool, error) {
	tag, err := r.p.Exec(ctx, "UPDATE hosts SET last_seen = $3 WHERE name = $1 AND stream_id = $2", host, stream, at)
	return tag.RowsAffected() > 0, err
}

// Detach records that stream ended: if it was still host's stream, the host is offline.
func (r *Registry) Detach(ctx context.Context, host, stream string) error {
	_, err := r.p.Exec(ctx, "UPDATE hosts SET stream_id = NULL, start_retry_keys = false WHERE name = $1 AND stream_id = $2", host, stream)
	return err
}

// Forget deletes everything host reported, in tx: it left its account.
func Forget(ctx context.Context, tx pgx.Tx, host string) error {
	for _, table := range []string{"hosts", "host_minutes", "agent_minutes", "oom_kills", "host_probes", "usage_active", "usage_sources"} {
		col := "host"
		if table == "hosts" { // its workspaces, agents, transcripts and commands go with it
			col = "name"
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE "+col+" = $1", host); err != nil {
			return err
		}
	}
	return nil
}
