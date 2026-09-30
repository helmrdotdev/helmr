package computer

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// RunProcess is one Run lease whose processes ran in an Instance
// incarnation.
type RunProcess struct {
	RunID         uuid.UUID
	RunLeaseID    uuid.UUID
	AttemptNumber uint32
}

// RunCleanup returns the oldest terminal Run lease of the writer's Instance
// incarnation whose processes are not yet reconciled, or nil when none is.
// Cleanup follows physical ownership, independently of an expired or
// terminal member lease; it cannot grant execution authority or close
// another member. It returns ErrAuthorityChanged when the principal no
// longer owns the mounted writer and workergroup.ErrStaleClaims when its
// claim versions changed.
func RunCleanup(ctx context.Context, txb db.TxBeginner, principal workergroup.HostPrincipal, writer WriterRef) (*RunProcess, error) {
	var process *RunProcess
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		i, err := lockCleanupWriter(ctx, tx, principal, writer)
		if err != nil {
			return err
		}
		var runID, leaseID string
		var attempt uint32
		err = tx.QueryRow(ctx, `SELECT run_id::text,id::text,attempt_number FROM run_leases
   WHERE computer_instance_id=$1 AND writer_generation=$2 AND process_reconciled_at IS NULL
   AND status IN ('completed','failed','cancelled','lost','rejected','expired')
   ORDER BY created_at,id LIMIT 1`, i.ID, i.WriterGeneration).Scan(&runID, &leaseID, &attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		run, err := uuid.Parse(runID)
		if err != nil {
			return fmt.Errorf("parse cleanup run ID: %w", err)
		}
		lease, err := uuid.Parse(leaseID)
		if err != nil {
			return fmt.Errorf("parse cleanup run lease ID: %w", err)
		}
		process = &RunProcess{RunID: run, RunLeaseID: lease, AttemptNumber: attempt}
		return nil
	})
	return process, authorityChanged(err)
}

// ReconcileRun records that the processes of a terminal Run lease in the
// writer's Instance incarnation have been cleaned up. A replay keeps the
// first reconciliation time. It returns ErrAuthorityChanged when the lease
// is not a terminal lease of that incarnation or the principal no longer
// owns the mounted writer, and workergroup.ErrStaleClaims when its claim
// versions changed.
func ReconcileRun(ctx context.Context, txb db.TxBeginner, principal workergroup.HostPrincipal, writer WriterRef, process RunProcess) error {
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		i, err := lockCleanupWriter(ctx, tx, principal, writer)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=COALESCE(process_reconciled_at,clock_timestamp())
   WHERE id=$1 AND run_id=$2 AND attempt_number=$3 AND computer_instance_id=$4 AND writer_generation=$5
   AND status IN ('completed','failed','cancelled','lost','rejected','expired')`, pgvalue.UUID(process.RunLeaseID), pgvalue.UUID(process.RunID), process.AttemptNumber, i.ID, i.WriterGeneration)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return nil
	})
	return authorityChanged(err)
}

// lockCleanupWriter locks group → host → Computer → Instance and requires the
// principal to hold the Instance's current, unexpired writer of a mounted,
// ready Instance whose admission is open or draining.
func lockCleanupWriter(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, writer WriterRef) (db.ComputerInstance, error) {
	if writer.WriterGeneration <= 0 {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	target, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: pgvalue.UUID(writer.InstanceID), EnvironmentID: pgvalue.UUID(writer.EnvironmentID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, q, principal)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.Continues() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: principal.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.WriterGeneration != writer.WriterGeneration || c.WriterGeneration != i.WriterGeneration || i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.MountState != "mounted" || (i.AdmissionState != "open" && i.AdmissionState != "draining") || !i.WriterExpiresAt.Time.After(now.Time) {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return i, nil
}
