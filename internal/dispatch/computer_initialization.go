package dispatch

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ComputerPreparationFence identifies a physical preparation, independent of members.
type ComputerPreparationFence struct {
	RuntimeID, WorkerID, WorkerGroupID pgtype.UUID
	WorkerEpoch, DesiredVersion        int64
}

// ComputerPreparation is valid only inside its owning transaction. Publication
// must recheck deadlines after object writes and before committing.
type ComputerPreparation struct {
	OrgID, ProjectID, EnvironmentID, ComputerID, VersionID pgtype.UUID
	WriterGeneration, LogicalBytes                         int64
	instance                                               db.ComputerInstance
	// admitting reports whether the locked supply could admit new work. Receipt
	// callers require it before a first restore activation.
	admitting bool
}

func LockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, preparationInitial)
}
func LockComputerSourcePreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, preparationSource)
}

// LockComputerReadyObservation also permits a Program-ready acknowledgement on
// an existing ready Instance; the physical preparation deadline applies only to
// the initial allocation. Worker, Computer and Instance fences still apply.
func LockComputerReadyObservation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, preparationReady)
}

type preparationObservation uint8

const (
	preparationInitial preparationObservation = iota
	preparationSource
	preparationReady
	preparationRestoreReceipt
)

// Draining or pause can follow a successful activation whose reply was lost.
// This mode permits receipt inspection on non-admitting supply; restore callers
// still require a committed checkpoint and create new activation authority only
// on admitting supply.
func lockComputerRestoreObservation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, preparationRestoreReceipt)
}

// lockComputerPreparation fences allocated Instances as admission: active
// supply without Run or VM pauses. Receipts and readiness of an Instance that is
// already ready continue admitted work: they accept paused or draining supply
// and ignore pauses, but a restoring Instance still needs admitting supply
// unless a receipt caller only inspects an already committed activation.
func lockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence, observation preparationObservation) (ComputerPreparation, error) {
	initial := observation == preparationInitial
	readiness := observation == preparationReady || observation == preparationRestoreReceipt
	receipt := observation == preparationRestoreReceipt

	var environmentID, computerID pgtype.UUID
	var region, observed string
	err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances
 WHERE id=$1 AND worker_host_id=$2 AND worker_group_id=$3 AND worker_epoch=$4`, fence.RuntimeID, fence.WorkerID, fence.WorkerGroupID, fence.WorkerEpoch).Scan(&environmentID, &computerID, &region, &observed)
	if err != nil {
		return ComputerPreparation{}, err
	}
	// The unlocked observation only selects the fence mode; the locked Instance
	// must still match it before the continuation exception applies.
	continuation := receipt || (readiness && observed == "ready")
	admitting, err := workergroup.LockPlacementSupply(ctx, tx, workergroup.PlacementSupply{GroupID: fence.WorkerGroupID, RegionID: region, HostID: fence.WorkerID, Epoch: fence.WorkerEpoch, RunArchitecture: runtimeArchitecture, Continuation: continuation})
	if err != nil {
		return ComputerPreparation{}, err
	}
	if !continuation && !admitting {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return ComputerPreparation{}, err
	}
	i, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: environmentID, ComputerID: computerID})
	if err != nil {
		return ComputerPreparation{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" ||
		i.ID != fence.RuntimeID || i.WorkerHostID != fence.WorkerID || i.WorkerGroupID != fence.WorkerGroupID || i.WorkerEpoch != fence.WorkerEpoch ||
		i.WriterGeneration != c.WriterGeneration || i.DesiredState != "ready" || i.DesiredVersion != fence.DesiredVersion ||
		(i.ObservedState != "ready" && (continuation || i.ObservedState != "allocated")) || (i.ObservedState == "ready" && !readiness) ||
		(i.AdmissionState != "open" && i.AdmissionState != "restoring" && !(continuation && i.AdmissionState == "draining")) ||
		(!receipt && !admitting && i.AdmissionState == "restoring") {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	version := i.SourceDiskVersionID
	if readiness && !version.Valid {
		version = c.HeadDiskVersionID
	}
	if initial {
		if version.Valid || i.SourceCheckpointID.Valid {
			return ComputerPreparation{}, pgx.ErrNoRows
		}
		version = c.HeadDiskVersionID
	}
	var root pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM computer_disk_versions WHERE environment_id=$1 AND computer_id=$2 AND id=$3
 AND (($4::boolean AND parent_version_id IS NULL AND status='initializing') OR (NOT $4::boolean AND status IN ('committed','private')))
 AND payload_not_retired`, environmentID, computerID, version, initial).Scan(&root)
	if err != nil {
		return ComputerPreparation{}, err
	}
	p := ComputerPreparation{OrgID: i.OrgID, ProjectID: i.ProjectID, EnvironmentID: environmentID, ComputerID: computerID, VersionID: root, WriterGeneration: i.WriterGeneration, LogicalBytes: i.ReservedGuestEphemeralDiskBytes, instance: i, admitting: admitting}
	if i.ObservedState == "ready" {
		writerLive, err := q.GetComputerInstanceWriterLive(ctx, db.GetComputerInstanceWriterLiveParams{ID: i.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
		if err != nil {
			return ComputerPreparation{}, err
		}
		if !writerLive {
			return ComputerPreparation{}, pgx.ErrNoRows
		}
		return p, nil
	}
	if err = p.CheckDeadlines(ctx, tx); err != nil {
		return ComputerPreparation{}, err
	}
	return p, nil
}

func (p ComputerPreparation) CheckDeadlines(ctx context.Context, tx pgx.Tx) error {
	valid, err := db.New(tx).GetComputerPreparationDeadlinesValid(ctx, db.GetComputerPreparationDeadlinesValidParams{ID: p.instance.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds, DesiredVersion: p.instance.DesiredVersion, WriterGeneration: p.instance.WriterGeneration})
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("Computer preparation expired: %w", pgx.ErrNoRows)
	}
	return nil
}

// RecordComputerInstanceReady acknowledges only the assigned VM shape and current
// observation. The caller owns commit/rollback. Frozen restore readiness does not
// finish preparation: that belongs to whole-Instance activation.
func RecordComputerInstanceReady(ctx context.Context, tx pgx.Tx, groupID pgtype.UUID, params db.MarkComputerInstanceReadyParams) (db.ComputerInstance, error) {
	preparation, err := LockComputerReadyObservation(ctx, tx, ComputerPreparationFence{RuntimeID: params.ID, WorkerID: params.WorkerHostID, WorkerGroupID: groupID, WorkerEpoch: params.WorkerEpoch, DesiredVersion: params.DesiredVersion})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	params.WriterGeneration = preparation.WriterGeneration
	q := db.New(tx)
	row, err := q.MarkComputerInstanceReady(ctx, params)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !row.SourceCheckpointID.Valid {
		var pending bool
		if err = tx.QueryRow(ctx, `SELECT coalesce(preparation_instance_id=$2,false) FROM computers WHERE id=$1`, row.ComputerID, row.ID).Scan(&pending); err != nil {
			return db.ComputerInstance{}, err
		}
		if pending {
			if _, err = q.CompleteComputerPreparation(ctx, db.CompleteComputerPreparationParams{EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID, InstanceID: row.ID, DesiredVersion: row.DesiredVersion}); err != nil {
				return db.ComputerInstance{}, err
			}
		}
	}
	return row, nil
}
