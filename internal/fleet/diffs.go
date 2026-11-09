package fleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
)

// RecordDiff stores a workspace's diff, received at at, in place of the one before.
func (r *Registry) RecordDiff(ctx context.Context, host string, d hostclient.WorkspaceDiff, at time.Time) error {
	files := d.Files
	if files == nil {
		files = []paseo.DiffFile{}
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return err
	}
	collected := d.CollectedAt
	if collected.IsZero() {
		collected = at
	}
	_, err = r.p.Exec(ctx, `INSERT INTO workspace_diffs (host, workspace_id, directory, error, too_large, file_count, additions,
		deletions, files, collected_at, received_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (host, workspace_id) DO UPDATE SET directory = EXCLUDED.directory, error = EXCLUDED.error,
		too_large = EXCLUDED.too_large, file_count = EXCLUDED.file_count, additions = EXCLUDED.additions,
		deletions = EXCLUDED.deletions, files = EXCLUDED.files, collected_at = EXCLUDED.collected_at,
		received_at = EXCLUDED.received_at`,
		host, d.WorkspaceID, d.Directory, d.Error, d.TooLarge, len(files), d.Additions, d.Deletions, raw, collected, at)
	return err
}
