package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Unknown projects reserve the fallback until their finder selects a repository. Persist the
// estimate with the run so retries and reservations use exactly what admission charged.
func repositoryMemory(ctx context.Context, tx pgx.Tx, account int64, host, repo string, selected bool) (int64, error) {
	const fallback = int64(4 << 30)
	if !selected {
		return fallback, nil
	}
	var estimate int64
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT estimated_memory_bytes FROM workflow_repository_memory
		WHERE account_id=$1 AND host=$2 AND repo=$3), $4)`, account, host, repo, fallback).Scan(&estimate)
	return estimate, err
}
