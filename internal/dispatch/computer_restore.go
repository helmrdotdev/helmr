package dispatch

import (
	"context"
	"encoding/json"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CommitComputerRestore grants activation to exactly one destination and all
// captured members on the computer.Restore fence. The caller owns rollback on
// failure and commit on success.
func (d *Authority) CommitComputerRestore(ctx context.Context, tx pgx.Tx, destination computer.InstanceRef) (db.ComputerCheckpoint, error) {
	q := db.New(tx)
	var checkpoint db.ComputerCheckpoint
	var environmentID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT environment_id FROM computer_instances WHERE id=$1`, pgvalue.UUID(destination.ID)).Scan(&environmentID); err != nil {
		return checkpoint, err
	}
	i, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: environmentID, ID: pgvalue.UUID(destination.ID)})
	if err != nil {
		return checkpoint, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: environmentID, CheckpointID: i.SourceCheckpointID})
	if err != nil {
		return checkpoint, err
	}
	// Restored members may span queues. Acquire the entire queue union before
	// Secrets or placement authority, using the same lock keys as fresh admission.
	var keys []int64
	for _, m := range members {
		r, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: environmentID, ID: m.RunID})
		if err != nil {
			return checkpoint, err
		}
		key, err := queueScopeLockKey(environmentID, r.QueueName, r.ConcurrencyKey)
		if err != nil {
			return checkpoint, err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return checkpoint, err
		}
	}
	// Snapshot memory retains Secret material. Revoked bindings must stop restore;
	// ErrDeliveryUnavailable requires terminal cleanup, not an admission retry.
	for _, m := range members {
		if _, err = secret.LockAttemptDelivery(ctx, q, m.RunID, m.AttemptNumber, m.ComputerID); err != nil {
			return checkpoint, err
		}
	}
	restore, err := computer.LockRestore(ctx, tx, destination)
	if err != nil {
		return checkpoint, err
	}
	i = restore.Instance()
	if !i.SourceCheckpointID.Valid || i.ObservedState != "ready" || i.MountState != "mounted" || i.ObservedDesiredVersion != i.DesiredVersion {
		return checkpoint, pgx.ErrNoRows
	}
	// Capture owns immutable membership. Lock live owners before inspecting their
	// current outcomes, so wakeups cannot race grant and wait rebinding.
	if err = restore.LockCommitMembers(ctx); err != nil {
		return checkpoint, err
	}
	// A committed receipt is replayed without allocating new leases or intents.
	var committed bool
	if err = tx.QueryRow(ctx, `SELECT resume_computer_instance_id=$2 AND resume_committed_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, i.SourceCheckpointID, i.ID).Scan(&committed); err != nil {
		return checkpoint, err
	}
	if committed {
		return q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: i.ComputerID, CheckpointID: i.SourceCheckpointID})
	}
	// A first commit starts Runs, so it needs admitting supply.
	if !restore.Admitting() || i.AdmissionState != "restoring" {
		return checkpoint, pgx.ErrNoRows
	}
	if _, err = q.GetComputerInstanceRestoreCheckpoint(ctx, db.GetComputerInstanceRestoreCheckpointParams{ComputerInstanceID: i.ID, EnvironmentID: environmentID, WorkerGroupID: i.WorkerGroupID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion}); err != nil {
		return checkpoint, err
	}
	checkpoint, err = q.CommitComputerCheckpointRestore(ctx, db.CommitComputerCheckpointRestoreParams{CheckpointID: i.SourceCheckpointID, EnvironmentID: environmentID, ComputerInstanceID: i.ID, WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion})
	if err != nil {
		return checkpoint, err
	}
	for _, m := range members {
		r, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: environmentID, ID: m.RunID})
		if err != nil {
			return checkpoint, err
		}
		wait, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: m.RunWaitID, RunID: r.ID, AttemptNumber: m.AttemptNumber})
		if err != nil {
			return checkpoint, err
		}
		if wait.ExpectedRunRevision != r.Revision || wait.CurrentRunLeaseID.Valid || r.ActiveElapsedMs >= r.MaxActiveDurationMs {
			return checkpoint, pgx.ErrNoRows
		}
		current, err := restoreWaitIsCurrent(ctx, tx, m.RunWaitID)
		if err != nil {
			return checkpoint, err
		}
		restorable := r.Status == db.RunStatusWaiting && (wait.SuspensionStatus == "parked" || wait.SuspensionStatus == "resume_pending") ||
			r.Status == db.RunStatusQueued && wait.SuspensionStatus == "resume_pending"
		if !current || !restorable || r.CurrentAttemptNumber != m.AttemptNumber || r.CurrentRunLeaseID.Valid {
			return checkpoint, pgx.ErrNoRows
		}
		lease, err := d.grantFreshRun(ctx, tx, r, i)
		if err != nil {
			return checkpoint, err
		}
		result, err := tx.Exec(ctx, `UPDATE run_waits w SET suspension_status='resuming',current_run_lease_id=$2,expected_run_revision=r.revision,updated_at=clock_timestamp()
 FROM runs r WHERE w.id=$1 AND w.run_id=r.id AND r.current_run_lease_id=$2
 AND w.suspend_checkpoint_id=$3 AND w.prior_run_lease_id=$4 AND w.suspension_status IN ('parked','resume_pending')`, m.RunWaitID, lease.ID, checkpoint.ID, m.SourceRunLeaseID)
		if err != nil {
			return checkpoint, err
		}
		if result.RowsAffected() != 1 {
			return checkpoint, pgx.ErrNoRows
		}
	}
	payload, err := json.Marshal(struct {
		CheckpointID       string `json:"checkpoint_id"`
		ComputerInstanceID string `json:"computer_instance_id"`
		DesiredVersion     int64  `json:"desired_version"`
		WriterGeneration   int64  `json:"writer_generation"`
	}{pgvalue.UUIDString(checkpoint.ID), pgvalue.UUIDString(i.ID), i.DesiredVersion, i.WriterGeneration})
	if err != nil {
		return checkpoint, err
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return checkpoint, err
	}
	_, err = q.CreateControlOutbox(ctx, db.CreateControlOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), Topic: computer.RestoreActivationTopic, Payload: payload, AvailableAt: now})
	if err != nil {
		return checkpoint, err
	}
	if err = restore.RecheckReady(ctx); err != nil {
		return checkpoint, err
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT i.preparation_expires_at>clock_timestamp()
 AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id AND
 (l.status<>'assigned' OR l.start_deadline_at<=clock_timestamp() OR l.expires_at<=clock_timestamp()))
 FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, i.ID).Scan(&live); err != nil {
		return checkpoint, err
	}
	if !live {
		return checkpoint, pgx.ErrNoRows
	}
	return checkpoint, nil
}

// Called with the captured Session, Run and wait locked. Session cancellation
// can precede asynchronous Run cancellation; neither admission stage may cross it.
func restoreWaitIsCurrent(ctx context.Context, tx pgx.Tx, waitID pgtype.UUID) (bool, error) {
	current, err := db.New(tx).RunWaitTurnCurrent(ctx, waitID)
	if err != nil || !current {
		return current, err
	}
	err = tx.QueryRow(ctx, `SELECT r.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions s WHERE s.id=r.session_id AND s.current_run_id=r.id AND s.status IN ('open','closing') AND s.cancel_requested_at IS NULL) FROM run_waits w JOIN runs r ON r.id=w.run_id WHERE w.id=$1`, waitID).Scan(&current)
	return current, err
}
