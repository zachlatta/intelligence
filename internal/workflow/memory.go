package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// finderMemory is what a run reserves while its finder chooses the project.
const finderMemory = int64(1 << 30)

// Unknown projects reserve finderMemory until their finder selects a repository. A selected
// project's estimate comes from the workflow's own runs in that repository
// (workflow_memory_estimate), so an email answer is not charged like a Factory Task in the same
// repository; with none yet, from the repository's runs (workflow_repository_memory); else 4 GiB.
// Persist the estimate with the run so retries and reservations use exactly what admission charged.
func repositoryMemory(ctx context.Context, tx pgx.Tx, account int64, host, workflowID, repo string, selected bool) (int64, error) {
	const fallback = int64(4 << 30)
	if !selected {
		// Only the finder runs until the project is chosen, and admission checks the project's
		// own estimate again then.
		return finderMemory, nil
	}
	var estimate int64
	err := tx.QueryRow(ctx, `SELECT coalesce(
		(SELECT estimated_memory_bytes FROM workflow_memory_estimate
			WHERE account_id=$1 AND host=$2 AND workflow_id::text=$5 AND repo=$3),
		(SELECT estimated_memory_bytes FROM workflow_repository_memory
			WHERE account_id=$1 AND host=$2 AND repo=$3), $4)`, account, host, repo, fallback, workflowID).Scan(&estimate)
	return estimate, err
}
