package computer

import (
	"context"
	"errors"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ExpireInstance revokes the physical authority of an expired Instance
// candidate in the caller's transaction under Computer → Instance locks, and
// dead-letters the restore activation intent of an unacknowledged
// destination it closes. It reports false, with nothing changed, when the
// candidate no longer expires. Only cleanup proof releases capacity and pins.
func ExpireInstance(ctx context.Context, tx pgx.Tx, candidate db.ComputerInstance) (bool, error) {
	q := db.New(tx)
	if _, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ComputerID}); err != nil {
		return false, err
	}
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM computer_instances WHERE id=$1 AND computer_id=$2 FOR UPDATE`, candidate.ID, candidate.ComputerID).Scan(&id); err != nil {
		return false, err
	}
	_, err := q.ExpireComputerInstance(ctx, db.ExpireComputerInstanceParams{ID: candidate.ID, EnvironmentID: candidate.EnvironmentID, WriterGeneration: candidate.WriterGeneration, DesiredVersion: candidate.DesiredVersion})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Closing an unacknowledged destination revokes delivery, not the one-shot
	// checkpoint commitment. A late Worker cannot activate this destination and
	// the durable intent must no longer remain eligible for delivery.
	if _, err = tx.Exec(ctx, `UPDATE control_outbox o SET status='dead_lettered',claimed_by=NULL,claim_expires_at=NULL,last_error='Computer restore destination expired'
 FROM computer_checkpoints cp,computer_instances i
 WHERE i.id=$1 AND i.desired_state='closed' AND cp.id=i.source_checkpoint_id
 AND cp.resume_computer_instance_id=i.id AND cp.resume_committed_at IS NOT NULL
 AND o.topic=$2 AND o.status IN ('pending','claimed')
 AND o.payload->>'checkpoint_id'=cp.id::text AND o.payload->>'computer_instance_id'=i.id::text
 AND o.payload->>'desired_version'=$3 AND o.payload->>'writer_generation'=$4`,
		candidate.ID, RestoreActivationTopic, strconv.FormatInt(candidate.DesiredVersion, 10), strconv.FormatInt(candidate.WriterGeneration, 10)); err != nil {
		return false, err
	}
	return true, nil
}

// SettlePreparation settles a failed preparation candidate in the caller's
// transaction under Computer → Instance locks: it charges the Computer's
// preparation budget and, when the budget is exhausted, invalidates the
// checkpoint the preparation restored. It reports false, with nothing
// changed, when the candidate no longer needs settlement. Physical fact
// producers do not own retry accounting: a committed terminal Instance stays
// a durable candidate even if its producer crashes.
func SettlePreparation(ctx context.Context, tx pgx.Tx, candidate db.ListFailedComputerPreparationsRow) (bool, error) {
	q := db.New(tx)
	if _, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ComputerID}); err != nil {
		return false, err
	}
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM computer_instances WHERE id=$1 AND computer_id=$2 FOR UPDATE`, candidate.InstanceID, candidate.ComputerID).Scan(&id); err != nil {
		return false, err
	}
	c, err := q.SettleComputerPreparationFailure(ctx, db.SettleComputerPreparationFailureParams{EnvironmentID: candidate.EnvironmentID, ComputerID: candidate.ComputerID, InstanceID: candidate.InstanceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(c.PreparationFailure) > 0 {
		if err = q.InvalidateExhaustedComputerCheckpoint(ctx, db.InvalidateExhaustedComputerCheckpointParams{EnvironmentID: c.EnvironmentID, ComputerID: c.ID}); err != nil {
			return false, err
		}
	}
	return true, nil
}
