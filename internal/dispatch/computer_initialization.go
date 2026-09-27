package dispatch

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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

// Draining can follow a successful activation whose reply was lost. This mode
// permits receipt inspection; restore callers still require a committed checkpoint
// and never create new activation authority for a draining Instance.
func lockComputerRestoreObservation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, preparationRestoreReceipt)
}

func lockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence, observation preparationObservation) (ComputerPreparation, error) {
	initial := observation == preparationInitial
	readiness := observation == preparationReady || observation == preparationRestoreReceipt
	receipt := observation == preparationRestoreReceipt

	var environmentID, computerID pgtype.UUID
	var region string
	err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,region_id FROM computer_instances
 WHERE id=$1 AND worker_host_id=$2 AND worker_group_id=$3 AND worker_epoch=$4`, fence.RuntimeID, fence.WorkerID, fence.WorkerGroupID, fence.WorkerEpoch).Scan(&environmentID, &computerID, &region)
	if err != nil {
		return ComputerPreparation{}, err
	}
	if err = lockWorkerFence(ctx, tx, workerFence{GroupID: fence.WorkerGroupID, RegionID: region, WorkerHostID: fence.WorkerID, WorkerEpoch: fence.WorkerEpoch, RunArchitecture: runtimeArchitecture, AllowDraining: receipt}); err != nil {
		return ComputerPreparation{}, err
	}
	if err = checkLockedWorkerRuntimeAdmission(ctx, tx, fence.WorkerID, fence.WorkerEpoch); err != nil {
		return ComputerPreparation{}, err
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
		i.WriterGeneration != c.WriterGeneration || i.DesiredState != "ready" || i.DesiredVersion != fence.DesiredVersion || (i.ObservedState != "allocated" && !(readiness && i.ObservedState == "ready")) ||
		(i.AdmissionState != "open" && i.AdmissionState != "restoring" && !(receipt && i.AdmissionState == "draining")) {
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
	p := ComputerPreparation{OrgID: i.OrgID, ProjectID: i.ProjectID, EnvironmentID: environmentID, ComputerID: computerID, VersionID: root, WriterGeneration: i.WriterGeneration, LogicalBytes: i.ReservedGuestEphemeralDiskBytes, instance: i}
	if readiness && i.ObservedState == "ready" {
		var writerLive bool
		if err = tx.QueryRow(ctx, `SELECT i.writer_expires_at>clock_timestamp()
 AND (w.status='active' OR ($3::boolean AND w.status='draining')) AND w.current_epoch=i.worker_epoch
 AND w.observed_at>=clock_timestamp()-$2*interval '1 second'
 FROM computer_instances i JOIN worker_hosts w ON w.id=i.worker_host_id WHERE i.id=$1`, i.ID, workerapi.WorkerObservationFreshnessSeconds, receipt && i.AdmissionState == "draining").Scan(&writerLive); err != nil {
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
	var valid bool
	err := tx.QueryRow(ctx, `SELECT i.preparation_expires_at>clock_timestamp() AND i.writer_expires_at>clock_timestamp()
 AND i.desired_state='ready' AND i.desired_version=$3 AND i.reclaimed_at IS NULL
 AND i.writer_generation=$4 AND i.observed_state='allocated'
 AND w.status='active' AND w.current_epoch=i.worker_epoch
 AND w.observed_at>=clock_timestamp()-$2*interval '1 second'
 FROM computer_instances i JOIN worker_hosts w ON w.id=i.worker_host_id WHERE i.id=$1`, p.instance.ID, workerapi.WorkerObservationFreshnessSeconds, p.instance.DesiredVersion, p.instance.WriterGeneration).Scan(&valid)
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
