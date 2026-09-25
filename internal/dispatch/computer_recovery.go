package dispatch

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// admitComputerRecoveryPreparation shares the Computer's budget across Run and
// process replacements. The caller already locks the Computer and inserts the
// new Runtime in this transaction; rejection rolls back that reservation too.
func admitComputerRecoveryPreparation(ctx context.Context, tx pgx.Tx, computerID, runtimeID, sourceID pgtype.UUID) error {
	var episode, source, priorRuntime pgtype.UUID
	var completed pgtype.Timestamptz
	var count int32
	var eligible bool
	var failed bool
	if err := tx.QueryRow(ctx, `SELECT recovery_id,recovery_version_id,recovery_runtime_id,recovery_completed_at,
		recovery_failure IS NOT NULL,recovery_preparation_count,next_recovery_preparation_at IS NULL OR next_recovery_preparation_at<=clock_timestamp()
		FROM computers WHERE id=$1 FOR UPDATE`, computerID).Scan(&episode, &source, &priorRuntime, &completed, &failed, &count, &eligible); err != nil {
		return err
	}
	if failed {
		return ErrCandidateChanged
	}
	if !episode.Valid || completed.Valid {
		return nil
	}
	if source != sourceID || count >= 8 || !eligible {
		return ErrCandidateChanged
	}
	var excluded bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM runtime_instances WHERE workspace_id=$1 AND id<>$2 AND reclaimed_at IS NULL)`, computerID, runtimeID).Scan(&excluded); err != nil {
		return err
	}
	if !excluded {
		return ErrCandidateChanged
	}
	_, err := tx.Exec(ctx, `UPDATE computers SET recovery_preparation_count=recovery_preparation_count+1,
		recovery_runtime_id=$2,next_recovery_preparation_at=clock_timestamp()+LEAST(60,power(2,recovery_preparation_count)) * interval '1 second',
		revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, computerID, runtimeID)
	return err
}
