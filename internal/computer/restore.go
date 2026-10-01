package computer

import (
	"context"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Restore is the fence of a checkpoint restore into one destination Instance:
// the worker supply, the Computer and the Instance are locked in the owning
// transaction. The restore commit and its acknowledgement build on it; they
// lock and transition the restored members themselves. A Restore is valid
// only inside the transaction that locked it.
type Restore struct {
	tx       pgx.Tx
	ref      InstanceRef
	observed observedInstance
}

// LockRestore locks worker supply, the Computer and the destination Instance
// in that order and checks the restore receipt fence: the Instance is the
// ready incarnation at the worker epoch and desired version, and the Computer's
// current writer. It accepts paused or draining supply so a committed
// restore can be inspected and acknowledged after its reply was lost;
// Admitting reports whether new activation authority may be created. A fence
// that no longer holds returns pgx.ErrNoRows.
func LockRestore(ctx context.Context, tx pgx.Tx, ref InstanceRef) (Restore, error) {
	observed, err := lockRestoreReceipt(ctx, tx, ref)
	if err != nil {
		return Restore{}, err
	}
	return Restore{tx: tx, ref: ref, observed: observed}, nil
}

// Instance is the locked destination Instance.
func (r Restore) Instance() db.ComputerInstance {
	return r.observed.instance
}

// Admitting reports whether the locked supply could admit new work, which a
// first restore commit and the activation that opens the Instance require.
func (r Restore) Admitting() bool {
	return r.observed.admitting
}

// Open opens admission of the restored Instance once its caller has started
// every restored member: it opens the Instance, completes the Computer's
// preparation when this Instance holds it, and records delivery of the
// Instance's restore activation intent. Opening admission and recording
// delivery commit together; an open Instance is replay evidence even after
// the delivered outbox row is pruned. A missing activation intent returns
// pgx.ErrNoRows.
func (r Restore) Open(ctx context.Context) (db.ComputerInstance, error) {
	i := r.observed.instance
	q := db.New(r.tx)
	i, err := q.OpenRestoredComputerInstance(ctx, db.OpenRestoredComputerInstanceParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if err = completePendingPreparation(ctx, r.tx, i); err != nil {
		return db.ComputerInstance{}, err
	}
	// The authenticated installation receipt completes this exact physical intent,
	// independently of a delivery worker's claim lifetime.
	delivered, err := r.tx.Exec(ctx, `UPDATE control_outbox SET status='delivered',claimed_by=NULL,claim_expires_at=NULL,last_error=NULL,delivered_at=clock_timestamp()
 WHERE topic=$1 AND status IN ('pending','claimed')
 AND payload->>'checkpoint_id'=$2 AND payload->>'computer_instance_id'=$3
 AND payload->>'desired_version'=$4 AND payload->>'writer_generation'=$5`, RestoreActivationTopic, pgvalue.UUIDString(i.SourceCheckpointID), pgvalue.UUIDString(i.ID), strconv.FormatInt(i.DesiredVersion, 10), strconv.FormatInt(i.WriterGeneration, 10))
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if delivered.RowsAffected() != 1 {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return i, nil
}

// LockCommitMembers locks the members that the restore commit grants, in
// stable order after the restore fence: the Sessions, Runs, Attempts and
// Waits the Instance's source checkpoint captured, then the checkpoint.
func (r Restore) LockCommitMembers(ctx context.Context) error {
	return r.lockMembers(ctx, []string{
		`SELECT s.id FROM sessions s WHERE s.id IN(SELECT r.session_id FROM runs r JOIN computer_checkpoint_runs m ON m.run_id=r.id WHERE m.checkpoint_id=$1) ORDER BY s.id FOR UPDATE`,
		`SELECT r.id FROM runs r JOIN computer_checkpoint_runs m ON m.run_id=r.id WHERE m.checkpoint_id=$1 ORDER BY r.id FOR UPDATE OF r`,
		`SELECT a.run_id FROM run_attempts a JOIN computer_checkpoint_runs m ON m.run_id=a.run_id AND m.attempt_number=a.number WHERE m.checkpoint_id=$1 ORDER BY a.run_id FOR UPDATE OF a`,
		`SELECT w.id FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id WHERE m.checkpoint_id=$1 ORDER BY w.run_id,w.id FOR UPDATE OF w`,
		`SELECT id FROM computer_checkpoints WHERE id=$1 FOR UPDATE`,
	})
}

// LockAcknowledgementMembers locks the members that the restore
// acknowledgement starts, in stable order after the restore fence: the
// commit's members with the Run leases granted on the Instance after the
// Attempts and the Session turns after the Waits, before the checkpoint.
func (r Restore) LockAcknowledgementMembers(ctx context.Context) error {
	return r.lockMembers(ctx, []string{
		`SELECT s.id FROM sessions s WHERE s.id IN(SELECT r.session_id FROM runs r JOIN computer_checkpoint_runs m ON m.run_id=r.id WHERE m.checkpoint_id=$1) ORDER BY s.id FOR UPDATE`,
		`SELECT r.id FROM runs r JOIN computer_checkpoint_runs m ON m.run_id=r.id WHERE m.checkpoint_id=$1 ORDER BY r.id FOR UPDATE OF r`,
		`SELECT a.run_id FROM run_attempts a JOIN computer_checkpoint_runs m ON m.run_id=a.run_id AND m.attempt_number=a.number WHERE m.checkpoint_id=$1 ORDER BY a.run_id FOR UPDATE OF a`,
		`SELECT l.id FROM run_leases l JOIN computer_checkpoints c ON c.resume_computer_instance_id=l.computer_instance_id WHERE c.id=$1 ORDER BY l.id FOR UPDATE OF l`,
		`SELECT w.id FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id WHERE m.checkpoint_id=$1 ORDER BY w.run_id,w.id FOR UPDATE OF w`,
		`SELECT t.id FROM session_turns t WHERE t.id IN(SELECT w.turn_id FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id WHERE m.checkpoint_id=$1) ORDER BY t.id FOR UPDATE`,
		`SELECT id FROM computer_checkpoints WHERE id=$1 FOR UPDATE`,
	})
}

func (r Restore) lockMembers(ctx context.Context, queries []string) error {
	for _, query := range queries {
		if _, err := r.tx.Exec(ctx, query, r.observed.instance.SourceCheckpointID); err != nil {
			return err
		}
	}
	return nil
}

// WaitCurrent reports whether a restored member's Wait still belongs to its
// current turn and Session. Called with the captured Session, Run and Wait
// locked. Session cancellation can precede asynchronous Run cancellation;
// neither admission stage may cross it.
func (r Restore) WaitCurrent(ctx context.Context, waitID pgtype.UUID) (bool, error) {
	current, err := db.New(r.tx).RunWaitTurnCurrent(ctx, waitID)
	if err != nil || !current {
		return current, err
	}
	err = r.tx.QueryRow(ctx, `SELECT r.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions s WHERE s.id=r.session_id AND s.current_run_id=r.id AND s.status IN ('open','closing') AND s.cancel_requested_at IS NULL) FROM run_waits w JOIN runs r ON r.id=w.run_id WHERE w.id=$1`, waitID).Scan(&current)
	return current, err
}

// RecheckReady re-evaluates the Instance's readiness fence, including its
// writer and preparation deadlines, after the caller's blocking writes may
// have consumed the remaining budget.
func (r Restore) RecheckReady(ctx context.Context) error {
	_, err := lockReadyObservation(ctx, r.tx, r.ref)
	return err
}

// completePendingPreparation completes the Computer's preparation when the
// ready Instance holds it.
func completePendingPreparation(ctx context.Context, tx pgx.Tx, i db.ComputerInstance) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(preparation_instance_id=$2,false) FROM computers WHERE id=$1`, i.ComputerID, i.ID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	_, err := db.New(tx).CompleteComputerPreparation(ctx, db.CompleteComputerPreparationParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID, InstanceID: i.ID, DesiredVersion: i.DesiredVersion})
	return err
}
