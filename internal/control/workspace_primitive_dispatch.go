package control

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspacePrimitiveOperationTTL  = 10 * time.Minute
	workspacePrimitiveWriteLeaseTTL = 24 * time.Hour
	workspaceDispatchListLimit      = int32(1000)

	workspaceOperationKindStartExec = db.WorkspaceMaterializationOperationKindStartExec
	workspaceOperationKindCreatePty = db.WorkspaceMaterializationOperationKindCreatePty
	workspaceOperationKindResizePty = db.WorkspaceMaterializationOperationKindResizePty
	workspaceOperationKindClosePty  = db.WorkspaceMaterializationOperationKindClosePty

	workspaceOperationResourceExec = db.WorkspaceResourceKindWorkspaceExec
	workspaceOperationResourcePty  = db.WorkspaceResourceKindWorkspacePty
)

type workspacePrimitiveOperationLease struct {
	writeLeaseID pgtype.UUID
	fencingToken string
}

type workspacePrimitiveResourceScope struct {
	orgID         pgtype.UUID
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	workspaceID   pgtype.UUID
}

type workspacePrimitiveControlTarget struct {
	name              string
	scope             workspacePrimitiveResourceScope
	materializationID pgtype.UUID
	resourceKind      db.WorkspaceResourceKind
	resourceID        pgtype.UUID
}

func enqueuePendingWorkspacePrimitiveOperations(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization) error {
	execs, err := store.ListWorkspaceExecsAwaitingDispatch(ctx, db.ListWorkspaceExecsAwaitingDispatchParams{
		OrgID:             materialization.OrgID,
		ProjectID:         materialization.ProjectID,
		EnvironmentID:     materialization.EnvironmentID,
		WorkspaceID:       materialization.WorkspaceID,
		MaterializationID: materialization.ID,
		LimitCount:        workspaceDispatchListLimit,
	})
	if err != nil {
		return err
	}
	for _, exec := range execs {
		exec, lease, err := ensureWorkspaceExecWriteLease(ctx, store, materialization, exec)
		if err != nil {
			return err
		}
		request, err := workspace.ExecStartOperationRequest(exec)
		if err != nil {
			return err
		}
		if err := requestWorkspacePrimitiveOperation(ctx, store, materialization, workspaceOperationKindStartExec, workspaceOperationResourceExec, exec.ID, request, lease); err != nil {
			return err
		}
	}
	ptys, err := store.ListWorkspacePtySessionsAwaitingDispatch(ctx, db.ListWorkspacePtySessionsAwaitingDispatchParams{
		OrgID:             materialization.OrgID,
		ProjectID:         materialization.ProjectID,
		EnvironmentID:     materialization.EnvironmentID,
		WorkspaceID:       materialization.WorkspaceID,
		MaterializationID: materialization.ID,
		LimitCount:        workspaceDispatchListLimit,
	})
	if err != nil {
		return err
	}
	for _, pty := range ptys {
		pty, lease, err := ensureWorkspacePtyWriteLease(ctx, store, materialization, pty)
		if err != nil {
			return err
		}
		request, err := workspace.PtyCreateOperationRequest(pty)
		if err != nil {
			return err
		}
		if err := requestWorkspacePrimitiveOperation(ctx, store, materialization, workspaceOperationKindCreatePty, workspaceOperationResourcePty, pty.ID, request, lease); err != nil {
			return err
		}
	}
	return nil
}

func materializationFromEnsureRow(row db.EnsureWorkspaceMaterializationRequestedRow) db.WorkspaceMaterialization {
	return db.WorkspaceMaterialization{
		ID:                          row.ID,
		OrgID:                       row.OrgID,
		ProjectID:                   row.ProjectID,
		EnvironmentID:               row.EnvironmentID,
		WorkspaceID:                 row.WorkspaceID,
		DeploymentSandboxID:         row.DeploymentSandboxID,
		SandboxFingerprint:          row.SandboxFingerprint,
		BaseVersionID:               row.BaseVersionID,
		WorkerInstanceID:            row.WorkerInstanceID,
		ReservationToken:            row.ReservationToken,
		ReservationExpiresAt:        row.ReservationExpiresAt,
		ClaimAttempt:                row.ClaimAttempt,
		DeadLetteredAt:              row.DeadLetteredAt,
		Priority:                    row.Priority,
		RequestedCpuMillis:          row.RequestedCpuMillis,
		RequestedMemoryMib:          row.RequestedMemoryMib,
		RequestedDiskMib:            row.RequestedDiskMib,
		RequestedExecutionSlots:     row.RequestedExecutionSlots,
		ReservedCpuMillis:           row.ReservedCpuMillis,
		ReservedMemoryMib:           row.ReservedMemoryMib,
		ReservedDiskMib:             row.ReservedDiskMib,
		ReservedExecutionSlots:      row.ReservedExecutionSlots,
		CapacityReservationID:       row.CapacityReservationID,
		GuestdChannelTokenHash:      row.GuestdChannelTokenHash,
		GuestdChannelTokenExpiresAt: row.GuestdChannelTokenExpiresAt,
		RuntimeID:                   row.RuntimeID,
		State:                       row.State,
		Request:                     row.Request,
		LeaseGeneration:             row.LeaseGeneration,
		DirtyGeneration:             row.DirtyGeneration,
		FencingGeneration:           row.FencingGeneration,
		NetworkNamespace:            row.NetworkNamespace,
		PortNamespace:               row.PortNamespace,
		ImageArtifactID:             row.ImageArtifactID,
		ImageArtifactFormat:         row.ImageArtifactFormat,
		RootfsDigest:                row.RootfsDigest,
		ImageDigest:                 row.ImageDigest,
		ImageFormat:                 row.ImageFormat,
		WorkspaceArtifactID:         row.WorkspaceArtifactID,
		WorkspaceArtifactEncoding:   row.WorkspaceArtifactEncoding,
		WorkspaceArtifactEntryCount: row.WorkspaceArtifactEntryCount,
		WorkspaceArtifactDigest:     row.WorkspaceArtifactDigest,
		WorkspaceArtifactSizeBytes:  row.WorkspaceArtifactSizeBytes,
		WorkspaceArtifactMediaType:  row.WorkspaceArtifactMediaType,
		WorkspaceMountPath:          row.WorkspaceMountPath,
		RuntimeABI:                  row.RuntimeABI,
		GuestdAbi:                   row.GuestdAbi,
		AdapterAbi:                  row.AdapterAbi,
		LastHeartbeatAt:             row.LastHeartbeatAt,
		RequestedAt:                 row.RequestedAt,
		MaterializedAt:              row.MaterializedAt,
		StoppedAt:                   row.StoppedAt,
		LostAt:                      row.LostAt,
		FailedAt:                    row.FailedAt,
		Error:                       row.Error,
		CreatedAt:                   row.CreatedAt,
		UpdatedAt:                   row.UpdatedAt,
	}
}

func requestWorkspacePrimitiveOperation(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization, operationKind db.WorkspaceMaterializationOperationKind, resourceKind db.WorkspaceResourceKind, resourceID pgtype.UUID, request []byte, lease workspacePrimitiveOperationLease) error {
	fingerprint, err := workspace.OperationFingerprint(operationKind, request)
	if err != nil {
		return err
	}
	_, err = store.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OperationKind:      operationKind,
		ResourceKind:       resourceKind,
		ResourceID:         resourceID,
		RequestFingerprint: fingerprint,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspacePrimitiveOperationTTL)),
		Priority:           0,
		InstanceLeaseID:    pgtype.UUID{},
		WriteLeaseID:       lease.writeLeaseID,
		FencingToken:       lease.fencingToken,
		Request:            request,
		OrgID:              materialization.OrgID,
		ProjectID:          materialization.ProjectID,
		EnvironmentID:      materialization.EnvironmentID,
		WorkspaceID:        materialization.WorkspaceID,
		MaterializationID:  materialization.ID,
	})
	if err == nil {
		return nil
	}
	if !isUniqueViolation(err) {
		return err
	}
	existing, getErr := store.GetActiveWorkspaceMaterializationOperationByResource(ctx, db.GetActiveWorkspaceMaterializationOperationByResourceParams{
		OrgID:             materialization.OrgID,
		ProjectID:         materialization.ProjectID,
		EnvironmentID:     materialization.EnvironmentID,
		WorkspaceID:       materialization.WorkspaceID,
		MaterializationID: materialization.ID,
		OperationKind:     operationKind,
		ResourceKind:      resourceKind,
		ResourceID:        resourceID,
	})
	if getErr != nil {
		return getErr
	}
	if existing.RequestFingerprint != fingerprint {
		return conflict(errors.New("active workspace primitive dispatch fingerprint mismatch"))
	}
	return nil
}

func ensureWorkspacePrimitiveWriterAvailable(ctx context.Context, store db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	if _, err := store.LockWorkspacePrimitiveWriterScope(ctx, db.LockWorkspacePrimitiveWriterScopeParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	}); err != nil {
		if isNoRows(err) {
			return conflict(errWorkspaceNotActive)
		}
		return err
	}
	hasWriter, err := store.WorkspaceHasActivePrimitiveWriter(ctx, db.WorkspaceHasActivePrimitiveWriterParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	if err != nil {
		return err
	}
	if hasWriter {
		return conflict(errWorkspaceWriterActive)
	}
	return nil
}

func workspacePrimitiveScopeForExec(row db.WorkspaceExec) workspacePrimitiveResourceScope {
	return workspacePrimitiveResourceScope{
		orgID:         row.OrgID,
		projectID:     row.ProjectID,
		environmentID: row.EnvironmentID,
		workspaceID:   row.WorkspaceID,
	}
}

func workspacePrimitiveScopeForPty(row db.WorkspacePtySession) workspacePrimitiveResourceScope {
	return workspacePrimitiveResourceScope{
		orgID:         row.OrgID,
		projectID:     row.ProjectID,
		environmentID: row.EnvironmentID,
		workspaceID:   row.WorkspaceID,
	}
}

func getWorkspacePrimitiveWriteLease(ctx context.Context, store db.Querier, scope workspacePrimitiveResourceScope, writeLeaseID pgtype.UUID) (workspacePrimitiveOperationLease, error) {
	lease, err := store.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
		OrgID:         scope.orgID,
		ProjectID:     scope.projectID,
		EnvironmentID: scope.environmentID,
		WorkspaceID:   scope.workspaceID,
		ID:            writeLeaseID,
	})
	if err != nil {
		return workspacePrimitiveOperationLease{}, err
	}
	return workspacePrimitiveOperationLease{writeLeaseID: lease.ID, fencingToken: lease.FencingToken}, nil
}

func acquireWorkspacePrimitiveWriteLease(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization, scope workspacePrimitiveResourceScope, ownerExecID pgtype.UUID, ownerPtySessionID pgtype.UUID) (workspacePrimitiveOperationLease, error) {
	lease, err := store.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       ownerExecID,
		OwnerPtySessionID: ownerPtySessionID,
		FencingToken:      uuid.Must(uuid.NewV7()).String(),
		HeartbeatToken:    uuid.Must(uuid.NewV7()).String(),
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(workspacePrimitiveWriteLeaseTTL)),
		OrgID:             scope.orgID,
		ProjectID:         scope.projectID,
		EnvironmentID:     scope.environmentID,
		WorkspaceID:       scope.workspaceID,
		MaterializationID: materialization.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return workspacePrimitiveOperationLease{}, conflict(errWorkspaceWriterActive)
		}
		return workspacePrimitiveOperationLease{}, err
	}
	return workspacePrimitiveOperationLease{writeLeaseID: lease.ID, fencingToken: lease.FencingToken}, nil
}

func ensureWorkspaceExecWriteLease(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization, row db.WorkspaceExec) (db.WorkspaceExec, workspacePrimitiveOperationLease, error) {
	scope := workspacePrimitiveScopeForExec(row)
	if row.WriteLeaseID.Valid {
		lease, err := getWorkspacePrimitiveWriteLease(ctx, store, scope, row.WriteLeaseID)
		if err != nil {
			return db.WorkspaceExec{}, workspacePrimitiveOperationLease{}, err
		}
		return row, lease, nil
	}
	lease, err := acquireWorkspacePrimitiveWriteLease(ctx, store, materialization, scope, row.ID, pgtype.UUID{})
	if err != nil {
		return db.WorkspaceExec{}, workspacePrimitiveOperationLease{}, err
	}
	row, err = store.BindWorkspaceExecMaterialization(ctx, db.BindWorkspaceExecMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   row.InstanceLeaseID,
		WriteLeaseID:      lease.writeLeaseID,
		State:             db.WorkspaceExecStateQueued,
		OrgID:             row.OrgID,
		ProjectID:         row.ProjectID,
		EnvironmentID:     row.EnvironmentID,
		WorkspaceID:       row.WorkspaceID,
		ID:                row.ID,
	})
	if err != nil {
		return db.WorkspaceExec{}, workspacePrimitiveOperationLease{}, err
	}
	return row, lease, nil
}

func ensureWorkspacePtyWriteLease(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization, row db.WorkspacePtySession) (db.WorkspacePtySession, workspacePrimitiveOperationLease, error) {
	scope := workspacePrimitiveScopeForPty(row)
	if row.WriteLeaseID.Valid {
		lease, err := getWorkspacePrimitiveWriteLease(ctx, store, scope, row.WriteLeaseID)
		if err != nil {
			return db.WorkspacePtySession{}, workspacePrimitiveOperationLease{}, err
		}
		return row, lease, nil
	}
	lease, err := acquireWorkspacePrimitiveWriteLease(ctx, store, materialization, scope, pgtype.UUID{}, row.ID)
	if err != nil {
		return db.WorkspacePtySession{}, workspacePrimitiveOperationLease{}, err
	}
	row, err = store.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   row.InstanceLeaseID,
		WriteLeaseID:      lease.writeLeaseID,
		State:             db.WorkspacePtyStateCreating,
		OrgID:             row.OrgID,
		ProjectID:         row.ProjectID,
		EnvironmentID:     row.EnvironmentID,
		WorkspaceID:       row.WorkspaceID,
		ID:                row.ID,
	})
	if err != nil {
		return db.WorkspacePtySession{}, workspacePrimitiveOperationLease{}, err
	}
	return row, lease, nil
}

func requestWorkspacePrimitiveControlOperation(ctx context.Context, store db.Querier, target workspacePrimitiveControlTarget, operationKind db.WorkspaceMaterializationOperationKind, request []byte, ensureWriteLease func(context.Context, db.Querier, db.WorkspaceMaterialization) (workspacePrimitiveOperationLease, error)) error {
	if !target.materializationID.Valid {
		return conflict(codedError{code: "workspace_materialization_not_runnable", message: target.name + " is not bound to a runnable materialization"})
	}
	materialization, err := store.GetWorkspaceMaterialization(ctx, db.GetWorkspaceMaterializationParams{
		OrgID:         target.scope.orgID,
		ProjectID:     target.scope.projectID,
		EnvironmentID: target.scope.environmentID,
		WorkspaceID:   target.scope.workspaceID,
		ID:            target.materializationID,
	})
	if err != nil {
		return err
	}
	if materialization.State != db.WorkspaceMaterializationStateRunning {
		return nil
	}
	lease, err := ensureWriteLease(ctx, store, materialization)
	if err != nil {
		return err
	}
	return requestWorkspacePrimitiveOperation(ctx, store, materialization, operationKind, target.resourceKind, target.resourceID, request, lease)
}

func requestWorkspacePtyControlOperation(ctx context.Context, store db.Querier, row db.WorkspacePtySession, operationKind db.WorkspaceMaterializationOperationKind, request []byte) error {
	target := workspacePrimitiveControlTarget{
		name:              "workspace pty",
		scope:             workspacePrimitiveScopeForPty(row),
		materializationID: row.MaterializationID,
		resourceKind:      workspaceOperationResourcePty,
		resourceID:        row.ID,
	}
	return requestWorkspacePrimitiveControlOperation(ctx, store, target, operationKind, request, func(ctx context.Context, store db.Querier, materialization db.WorkspaceMaterialization) (workspacePrimitiveOperationLease, error) {
		_, lease, err := ensureWorkspacePtyWriteLease(ctx, store, materialization, row)
		return lease, err
	})
}
