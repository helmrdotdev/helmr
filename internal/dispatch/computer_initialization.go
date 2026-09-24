package dispatch

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ComputerPreparationFence identifies one physical preparation, not a user lease.
type ComputerPreparationFence struct {
	RuntimeID, WorkerID, WorkerGroupID pgtype.UUID
	WorkerEpoch, DesiredVersion        int64
}

// ComputerPreparation is valid only inside the transaction that acquired it.
// The CP owns that transaction and must recheck deadlines before committing its
// candidate registration/publication. It conveys no guest execution permission.
type ComputerPreparation struct {
	OrgID, ProjectID, EnvironmentID, ComputerID, VersionID pgtype.UUID
	OwnershipGeneration, WriterGeneration, LogicalBytes    int64
	run                                                    runPlacementAuthority
	runtime                                                runRuntime
}

// LockComputerPreparation uses the same Run/exec -> Computer -> supply -> Runtime
// order as placement. Discovery is speculative; all mutable authority is reread
// after taking its owning lock. Finalization authority is deliberately not reused.
func LockComputerPreparation(ctx context.Context, tx pgx.Tx, fence ComputerPreparationFence) (ComputerPreparation, error) {
	var org, computer, runID, processID pgtype.UUID
	var runRevision, processRevision pgtype.Int8
	err := tx.QueryRow(ctx, `SELECT r.org_id,r.workspace_id,r.reserved_run_id,r.reserved_process_id,
        runs.revision,p.revision FROM runtime_instances r
        LEFT JOIN runs ON runs.id=r.reserved_run_id
        LEFT JOIN workspace_processes p ON p.id=r.reserved_process_id
        WHERE r.id=$1 AND r.worker_instance_id=$2 AND r.worker_group_id=$3 AND r.worker_epoch=$4`,
		fence.RuntimeID, fence.WorkerID, fence.WorkerGroupID, fence.WorkerEpoch).Scan(&org, &computer, &runID, &processID, &runRevision, &processRevision)
	if err != nil {
		return ComputerPreparation{}, err
	}
	var authority runPlacementAuthority
	var exec workspaceExecAuthority
	if runID.Valid && !processID.Valid && runRevision.Valid {
		candidate := ReadyRunCandidate{OrgID: org, RunID: runID, ExpectedRunRevision: runRevision.Int64}
		if err = lockRunSecrets(ctx, tx, candidate); err != nil {
			return ComputerPreparation{}, err
		}
		authority, err = lockRunPlacementAuthority(ctx, tx, candidate, true)
		if err != nil {
			return ComputerPreparation{}, err
		}
		if authority.restoreCheckpointID.Valid || authority.sameWorkspaceChildWaitID.Valid {
			return ComputerPreparation{}, pgx.ErrNoRows
		}
	} else if processID.Valid && !runID.Valid && processRevision.Valid {
		candidate := ReadyWorkspaceExecCandidate{OrgID: org, ProcessID: processID, ExpectedRevision: processRevision.Int64}
		if err = lockWorkspaceExecSecrets(ctx, tx, candidate); err != nil {
			return ComputerPreparation{}, err
		}
		exec, err = lockWorkspaceExecAuthority(ctx, tx, candidate)
		if err != nil {
			return ComputerPreparation{}, err
		}
		authority = exec.runAuthority()
		authority.ownershipGeneration = exec.ownershipGeneration
		authority.writerGeneration = exec.writerGeneration
	} else {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	if authority.workspaceID != computer {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	runtime, err := discoverRunRuntime(ctx, tx, computer)
	if err != nil {
		return ComputerPreparation{}, err
	}
	if runtime.id != fence.RuntimeID || runtime.workerID != fence.WorkerID || runtime.groupID != fence.WorkerGroupID || runtime.workerEpoch != fence.WorkerEpoch {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	if err = lockWorkerFence(ctx, tx, workerFence{GroupID: runtime.groupID, RegionID: authority.regionID, WorkerInstanceID: runtime.workerID, WorkerEpoch: runtime.workerEpoch, RunArchitecture: authority.architecture}); err != nil {
		return ComputerPreparation{}, err
	}
	if err = checkLockedWorkerRuntimeAdmission(ctx, tx, runtime.workerID, runtime.workerEpoch); err != nil {
		return ComputerPreparation{}, err
	}
	runtime, err = lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		return ComputerPreparation{}, err
	}
	if runtime.desiredState != db.RuntimeDesiredStateReady || runtime.desiredVersion != fence.DesiredVersion || runtime.observedState != db.RuntimeObservedStateAllocated || runtime.restoreCheckpoint.Valid || !runtime.reservationActive {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	if runID.Valid {
		err = validateRunRuntime(authority, runtime)
	} else {
		err = validateWorkspaceExecRuntime(exec, runtime)
	}
	if err != nil {
		return ComputerPreparation{}, err
	}
	if runtime.reservedRunID != runID || runtime.reservedProcessID != processID || runtime.reservedVersionID != authority.baseWorkspaceVersionID {
		return ComputerPreparation{}, pgx.ErrNoRows
	}
	var root pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT v.id FROM computer_versions v JOIN computers w ON w.id=v.workspace_id
        WHERE v.environment_id=$1 AND v.workspace_id=$2 AND v.id=$3 AND w.head_version_id=v.id
        AND v.parent_version_id IS NULL AND v.status='initializing'`, authority.environmentID, computer, authority.baseWorkspaceVersionID).Scan(&root)
	if err != nil {
		return ComputerPreparation{}, err
	}
	return ComputerPreparation{OrgID: authority.orgID, ProjectID: authority.projectID, EnvironmentID: authority.environmentID, ComputerID: computer, VersionID: root,
		OwnershipGeneration: authority.ownershipGeneration, WriterGeneration: authority.writerGeneration, LogicalBytes: runtime.guestEphemeralDiskBytes, run: authority, runtime: runtime}, nil
}

// CheckDeadlines is called after candidate/artifact writes, before committing.
// A lock wait must not turn expired preparation into a fresh publication.
func (p ComputerPreparation) CheckDeadlines(ctx context.Context, tx pgx.Tx) error {
	if p.run.runID.Valid {
		if err := checkRunPreparationDeadlines(ctx, tx, p.run); err != nil {
			return err
		}
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT r.preparation_expires_at>clock_timestamp()
        AND w.observed_at>=clock_timestamp()-$2*interval '1 second'
        FROM runtime_instances r JOIN worker_instances w ON w.id=r.worker_instance_id WHERE r.id=$1`, p.runtime.id, workerapi.WorkerObservationFreshnessSeconds).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("computer preparation deadline expired: %w", pgx.ErrNoRows)
	}
	return nil
}
