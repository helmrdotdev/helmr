package computer

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// observedInstance is an Instance whose readiness fence holds in the owning
// transaction: supply, Computer and Instance rows are locked.
type observedInstance struct {
	instance db.ComputerInstance
	// admitting reports whether the locked supply could admit new work. A
	// restore requires it before its first commit and activation.
	admitting bool
}

// lockReadyObservation fences a readiness observation. It also permits a
// Program-ready acknowledgement on an existing ready Instance; the physical
// preparation deadline applies only to the initial allocation. Worker,
// Computer and Instance fences still apply.
func lockReadyObservation(ctx context.Context, tx pgx.Tx, ref InstanceRef) (observedInstance, error) {
	return lockObservation(ctx, tx, ref, false)
}

// lockRestoreReceipt fences a restore commit or acknowledgement. Draining or
// pause can follow a successful activation whose reply was lost, so it
// permits receipt inspection on non-admitting supply; restore callers still
// require a committed checkpoint and create new activation authority only on
// admitting supply.
func lockRestoreReceipt(ctx context.Context, tx pgx.Tx, ref InstanceRef) (observedInstance, error) {
	return lockObservation(ctx, tx, ref, true)
}

// lockObservation locks supply, the Computer and its Instance. Readiness of
// an allocated Instance opens admission: it needs active supply without Run
// or VM pauses and a live preparation deadline. Readiness of an Instance that
// is already ready and restore receipts continue admitted work: they accept
// paused or draining supply and ignore pauses, but a restoring Instance still
// needs admitting supply unless a receipt only inspects an already committed
// activation.
func lockObservation(ctx context.Context, tx pgx.Tx, ref InstanceRef, receipt bool) (observedInstance, error) {
	instanceID, workerID, groupID := pgvalue.UUID(ref.ID), pgvalue.UUID(ref.Host.HostID), pgvalue.UUID(ref.Host.GroupID)
	var environmentID, computerID pgtype.UUID
	var region, observed string
	err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances
 WHERE id=$1 AND worker_host_id=$2 AND worker_group_id=$3 AND worker_epoch=$4`, instanceID, workerID, groupID, ref.Host.Epoch).Scan(&environmentID, &computerID, &region, &observed)
	if err != nil {
		return observedInstance{}, err
	}
	// The unlocked observation only selects the fence mode; the locked Instance
	// must still match it before the continuation exception applies.
	continuation := receipt || observed == "ready"
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: groupID, RegionID: region, HostID: workerID, Epoch: ref.Host.Epoch, RunArchitecture: string(definition.ArchitectureX8664), Continuation: continuation})
	if err != nil {
		return observedInstance{}, err
	}
	if !continuation && !admitting {
		return observedInstance{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return observedInstance{}, err
	}
	i, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: environmentID, ComputerID: computerID})
	if err != nil {
		return observedInstance{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" ||
		i.ID != instanceID || i.WorkerHostID != workerID || i.WorkerGroupID != groupID || i.WorkerEpoch != ref.Host.Epoch ||
		i.WriterGeneration != c.WriterGeneration || i.DesiredState != "ready" || i.DesiredVersion != ref.DesiredVersion ||
		(i.ObservedState != "ready" && (continuation || i.ObservedState != "allocated")) ||
		(i.AdmissionState != "open" && i.AdmissionState != "restoring" && !(continuation && i.AdmissionState == "draining")) ||
		(!receipt && !admitting && i.AdmissionState == "restoring") {
		return observedInstance{}, pgx.ErrNoRows
	}
	version := i.SourceDiskVersionID
	if !version.Valid {
		version = c.HeadDiskVersionID
	}
	var root pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM computer_disk_versions WHERE environment_id=$1 AND computer_id=$2 AND id=$3
 AND (($4::boolean AND parent_version_id IS NULL AND status='initializing') OR (NOT $4::boolean AND status IN ('committed','private')))
 AND payload_not_retired`, environmentID, computerID, version, false).Scan(&root)
	if err != nil {
		return observedInstance{}, err
	}
	o := observedInstance{instance: i, admitting: admitting}
	if i.ObservedState == "ready" {
		writerLive, err := q.GetComputerInstanceWriterLive(ctx, db.GetComputerInstanceWriterLiveParams{ID: i.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
		if err != nil {
			return observedInstance{}, err
		}
		if !writerLive {
			return observedInstance{}, pgx.ErrNoRows
		}
		return o, nil
	}
	if err = checkPreparationDeadlines(ctx, tx, i); err != nil {
		return observedInstance{}, err
	}
	return o, nil
}

// checkPreparationDeadlines evaluates the preparation, writer and worker
// observation deadlines after every blocking lock.
func checkPreparationDeadlines(ctx context.Context, tx pgx.Tx, i db.ComputerInstance) error {
	valid, err := db.New(tx).GetComputerPreparationDeadlinesValid(ctx, db.GetComputerPreparationDeadlinesValidParams{ID: i.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds, DesiredVersion: i.DesiredVersion, WriterGeneration: i.WriterGeneration})
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("computer preparation expired: %w", pgx.ErrNoRows)
	}
	return nil
}
