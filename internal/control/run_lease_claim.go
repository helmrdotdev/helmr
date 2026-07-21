package control

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleRunLeaseClaim = errors.New("run lease claim is stale")

type runLeaseClaimAuthority struct {
	run            db.Run
	workspace      db.Workspace
	attempt        db.RunAttempt
	workerGroup    db.WorkerGroup
	worker         db.WorkerInstance
	networkSlot    db.WorkerNetworkSlot
	runtime        db.RuntimeInstance
	runLease       db.RunLease
	workspaceMount db.WorkspaceMount
	workspaceLease db.WorkspaceLease
}

func (s *Server) claimFreshTaskRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
		ID:                    leaseID,
		LeaseSequence:         leaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimFreshTaskRunLeaseInTx(ctx, work.q, worker, leaseID, leaseSequence, locators)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func claimFreshTaskRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	var authority runLeaseClaimAuthority
	var err error
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID:            locators.RunID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID:            locators.WorkspaceID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		RegionID:      locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorkspace(authority.run, authority.workspace); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       locators.RunID,
		Number:      locators.AttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID:       worker.WorkerGroupID,
		RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workerGroup.State != db.WorkerGroupStateActive ||
		!authority.workerGroup.AllowsRun ||
		authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.networkSlot, err = q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID:                locators.NetworkSlotID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		Generation:        locators.NetworkSlotGeneration,
		RuntimeInstanceID: locators.RuntimeInstanceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.networkSlot.State != db.WorkerNetworkSlotStateBound {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID:               locators.RuntimeInstanceID,
		OrgID:            locators.OrgID,
		ProjectID:        locators.ProjectID,
		EnvironmentID:    locators.EnvironmentID,
		RegionID:         locators.RegionID,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.runLease, err = q.LockRunLeaseClaimLease(ctx, db.LockRunLeaseClaimLeaseParams{
		ID:            leaseID,
		RunID:         locators.RunID,
		WorkspaceID:   locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber,
		LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID:                locators.WorkspaceMountID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID:                locators.WorkspaceLeaseID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
		WorkspaceMountID:  locators.WorkspaceMountID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorkspaceAuthority(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	if authority.runLease.State == db.RunLeaseStateAssigned {
		authority.runLease, err = q.MarkRunLeaseStarting(ctx, db.MarkRunLeaseStartingParams{
			ID:                    leaseID,
			LeaseSequence:         leaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
	}
	return authority, nil
}

func validateClaimWorkspace(run db.Run, workspace db.Workspace) error {
	if workspace.State != db.WorkspaceStateActive ||
		workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return errStaleRunLeaseClaim
	}
	if run.EntrypointKind != "task" ||
		run.ActorID.Valid ||
		workspace.OwnerRunID != run.ID ||
		workspace.OwnerActorID.Valid {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimWorker(authenticated workerActor, worker db.WorkerInstance) error {
	if !worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != authenticated.WorkerEpoch ||
		worker.ClaimVersion != authenticated.ClaimVersion ||
		worker.ProtocolVersion != authenticated.ProtocolVersion ||
		!worker.SupportsRun ||
		(worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining) {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimPhysicalAuthority(worker workerActor, authority runLeaseClaimAuthority) error {
	lease := authority.runLease
	runtime := authority.runtime
	if lease.WorkerGroupID != worker.WorkerGroupID ||
		lease.WorkerInstanceID != pgvalue.UUID(worker.WorkerInstanceID) ||
		lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.WorkerProtocolVersion != worker.ProtocolVersion ||
		lease.RuntimeInstanceID != runtime.ID ||
		lease.NetworkSlotID != authority.networkSlot.ID ||
		lease.NetworkSlotGeneration != authority.networkSlot.Generation ||
		lease.RuntimeIdentityID != runtime.RuntimeIdentityID {
		return errStaleRunLeaseClaim
	}
	if lease.State == db.RunLeaseStateAssigned && authority.worker.State != db.WorkerInstanceStateActive {
		return errStaleRunLeaseClaim
	}
	if runtime.DesiredState != db.RuntimeDesiredStateReady ||
		runtime.ObservedState != db.RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion ||
		runtime.ReclaimedAt.Valid ||
		runtime.ProgramDeploymentID != authority.run.DeploymentID ||
		runtime.DeploymentDefinitionID != authority.workspace.DeploymentDefinitionID ||
		runtime.ReservedRunID.Valid ||
		runtime.ReservedAttemptNumber.Valid ||
		runtime.ReservedProcessID.Valid ||
		runtime.ReservedWorkspaceVersionID.Valid ||
		runtime.ReservationExpiresAt.Valid {
		return errStaleRunLeaseClaim
	}
	if !authority.worker.RuntimeIdentityID.Valid ||
		authority.worker.RuntimeIdentityID.String != runtime.RuntimeIdentityID ||
		runtime.ReservedCpuMillis != lease.RequestedCpuMillis ||
		runtime.ReservedMemoryBytes != lease.RequestedMemoryBytes ||
		runtime.ReservedWorkloadDiskBytes != lease.RequestedWorkloadDiskBytes ||
		runtime.ReservedScratchBytes != lease.RequestedScratchBytes ||
		runtime.ReservedExecutionSlots != lease.RequestedExecutionSlots ||
		authority.worker.PerVmCpuMillis != lease.RequestedCpuMillis ||
		authority.worker.PerVmMemoryBytes != lease.RequestedMemoryBytes ||
		authority.worker.PerVmWorkloadDiskBytes != lease.RequestedWorkloadDiskBytes ||
		authority.worker.PerVmScratchBytes != lease.RequestedScratchBytes {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimWorkspaceAuthority(authority runLeaseClaimAuthority) error {
	mount := authority.workspaceMount
	lease := authority.workspaceLease
	if mount.State != db.WorkspaceMountStateMounted ||
		lease.State != db.WorkspaceLeaseStateActive ||
		lease.OwnerRunLeaseID != authority.runLease.ID ||
		lease.OwnerProcessID.Valid ||
		lease.BaseVersionID != authority.attempt.BaseWorkspaceVersionID ||
		lease.BaseVersionID != mount.MaterializedVersionID ||
		lease.MountFencingGeneration != mount.FencingGeneration ||
		lease.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		lease.WriterGeneration != authority.workspace.WriterGeneration {
		return errStaleRunLeaseClaim
	}
	return nil
}

func staleRunLeaseClaim(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errStaleRunLeaseClaim
	}
	return err
}
