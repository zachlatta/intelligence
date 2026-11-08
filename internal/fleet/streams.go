package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attach makes stream (a uuid) box's current stream, seen at at, and announces it, so the stream
// it had before ends on whichever server instance holds it. A box that has never reported gets
// its row here.
func (r *Registry) Attach(ctx context.Context, box, stream string, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO devboxes (name, last_seen, stream_id) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET last_seen = EXCLUDED.last_seen, stream_id = EXCLUDED.stream_id`,
			box, at, stream); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, notifySQL, StreamTopic(box))
		return err
	})
}

// Seen records that stream was heard from at at, and reports whether it is still box's stream
// (false: another stream has attached since, and this one should end).
func (r *Registry) Seen(ctx context.Context, box, stream string, at time.Time) (bool, error) {
	tag, err := r.p.Exec(ctx, "UPDATE devboxes SET last_seen = $3 WHERE name = $1 AND stream_id = $2", box, stream, at)
	return tag.RowsAffected() > 0, err
}

// Detach records that stream ended: if it was still box's stream, the box is offline.
func (r *Registry) Detach(ctx context.Context, box, stream string) error {
	_, err := r.p.Exec(ctx, "UPDATE devboxes SET stream_id = NULL WHERE name = $1 AND stream_id = $2", box, stream)
	return err
}
