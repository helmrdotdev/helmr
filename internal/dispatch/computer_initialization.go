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
}

func LockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, true)
}
func LockComputerSourcePreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	return lockComputerPreparation(ctx, tx, fence, false)
}

// lockComputerPreparation fences the preparation of an allocated Instance as
// admission: active supply without Run or VM pauses. Initial preparation
// writes the Computer's initializing head; source preparation reads the
// Instance's committed or private source version.
func lockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence, initial bool) (ComputerPreparation, error) {
	var environmentID, computerID pgtype.UUID
	var region, observed string
	err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances
 WHERE id=$1 AND worker_host_id=$2 AND worker_group_id=$3 AND worker_epoch=$4`, fence.RuntimeID, fence.WorkerID, fence.WorkerGroupID, fence.WorkerEpoch).Scan(&environmentID, &computerID, &region, &observed)
	if err != nil {
		return ComputerPreparation{}, err
	}
	admitting, err := workergroup.LockPlacementSupply(ctx, tx, workergroup.PlacementSupply{GroupID: fence.WorkerGroupID, RegionID: region, HostID: fence.WorkerID, Epoch: fence.WorkerEpoch, RunArchitecture: runtimeArchitecture})
	if err != nil {
		return ComputerPreparation{}, err
	}
	if !admitting {
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
		i.ObservedState != "allocated" || (i.AdmissionState != "open" && i.AdmissionState != "restoring") {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	version := i.SourceDiskVersionID
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
