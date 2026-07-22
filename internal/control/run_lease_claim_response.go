package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type runLeaseClaimProjection struct {
	program             db.GetDeploymentProgramAuthorityRow
	definition          db.DeploymentDefinition
	resetTarget         db.GetWorkspaceResetTargetAuthorityRow
	checkpointArtifacts []db.ListRunCheckpointArtifactAuthorityRow
}

type runLeaseClaimResponseAuthority struct {
	mode           runLeaseClaimMode
	restoreSource  runLeaseRestoreSource
	actor          db.Actor
	childRun       db.Run
	run            db.Run
	attempt        db.RunAttempt
	runtime        db.RuntimeInstance
	networkSlot    db.WorkerNetworkSlot
	runLease       db.RunLease
	workspace      db.Workspace
	workspaceMount db.WorkspaceMount
	workspaceLease db.WorkspaceLease
	enclosingWait  db.RunWait
	runWait        db.RunWait
	checkpoint     db.RunCheckpoint
}

type SecretDeliveryOpener interface {
	OpenDeliveries(uuid.UUID, []secret.DeliveryEnvelope) ([]secret.DeliveryMaterial, error)
}

func loadRunLeaseClaimProjection(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimResponseAuthority,
) (runLeaseClaimProjection, error) {
	program, err := store.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
		EnvironmentID: authority.run.EnvironmentID,
		DeploymentID:  authority.run.DeploymentID,
	})
	if err != nil {
		return runLeaseClaimProjection{}, fmt.Errorf("load Run Lease Program authority: %w", err)
	}
	definition, err := store.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: authority.run.EnvironmentID,
		DeploymentID:  authority.run.DeploymentID,
		Kind:          authority.run.EntrypointKind,
		DeclaredID:    authority.run.EntrypointDeclaredID,
	})
	if err != nil {
		return runLeaseClaimProjection{}, fmt.Errorf("load Run Lease declaration authority: %w", err)
	}
	projection := runLeaseClaimProjection{
		program:    program,
		definition: definition,
	}
	projection.resetTarget, err = store.GetWorkspaceResetTargetAuthority(
		ctx,
		db.GetWorkspaceResetTargetAuthorityParams{
			OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
			EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
			VersionID: authority.workspaceLease.BaseVersionID,
		},
	)
	if err != nil {
		return runLeaseClaimProjection{}, fmt.Errorf("load Run Lease Workspace Reset target authority: %w", err)
	}
	if authority.restoreSource == runLeaseRestoreRecreated {
		projection.checkpointArtifacts, err = store.ListRunCheckpointArtifactAuthority(
			ctx,
			authority.checkpoint.ID,
		)
		if err != nil {
			return runLeaseClaimProjection{}, fmt.Errorf("load Run Lease checkpoint Artifact authority: %w", err)
		}
	}
	return projection, nil
}

func projectRunLeaseClaimResponse(
	authority runLeaseClaimResponseAuthority,
	envelopes []secret.DeliveryEnvelope,
	projection runLeaseClaimProjection,
	buildPolicy *deployment.BuildPolicy,
	secretDelivery SecretDeliveryOpener,
	fencingKeys workspace.FencingKeys,
) (api.WorkerRunLeaseClaimResponse, error) {
	physical := runLeaseProjectionAuthority{
		run:            authority.run,
		attempt:        authority.attempt,
		runtime:        authority.runtime,
		networkSlot:    authority.networkSlot,
		runLease:       authority.runLease,
		workspace:      authority.workspace,
		workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	}
	lease, err := projectRunLeaseReceipt(physical)
	if err != nil {
		return api.WorkerRunLeaseClaimResponse{}, err
	}
	program, err := projectDeploymentProgram(projection.program, buildPolicy)
	if err != nil {
		return api.WorkerRunLeaseClaimResponse{}, err
	}
	if projection.program.DeploymentID != authority.run.DeploymentID ||
		projection.program.EnvironmentID != authority.run.EnvironmentID {
		return api.WorkerRunLeaseClaimResponse{}, errors.New("Run Lease Program authority is inconsistent")
	}
	var actor *db.Actor
	if authority.actor.ID.Valid {
		actor = &authority.actor
	}
	execution, err := projectRunLeaseExecution(runLeaseExecutionProjection{
		mode:                authority.mode,
		restoreSource:       authority.restoreSource,
		run:                 authority.run,
		attempt:             authority.attempt,
		actor:               actor,
		definition:          projection.definition,
		deploymentVersion:   projection.program.DeploymentVersion,
		runtime:             authority.runtime,
		workspaceMount:      authority.workspaceMount,
		enclosingWait:       authority.enclosingWait,
		runWait:             authority.runWait,
		checkpoint:          authority.checkpoint,
		childRun:            authority.childRun,
		checkpointArtifacts: projection.checkpointArtifacts,
	})
	if err != nil {
		return api.WorkerRunLeaseClaimResponse{}, err
	}
	capability, err := deriveWorkspaceCapability(fencingKeys, authority.workspaceLease)
	if err != nil {
		return api.WorkerRunLeaseClaimResponse{}, err
	}
	attachment, err := projectWorkspaceAttachment(physical, capability.Token, projection.resetTarget)
	if err != nil {
		return api.WorkerRunLeaseClaimResponse{}, err
	}
	secrets := make([]api.WorkerSecretDelivery, 0)
	if authority.mode == runLeaseClaimFresh || authority.mode == runLeaseClaimAttachChild {
		if secretDelivery == nil {
			return api.WorkerRunLeaseClaimResponse{}, errors.New("Secret delivery opener is not configured")
		}
		environmentID, err := pgvalue.UUIDValue(authority.run.EnvironmentID)
		if err != nil {
			return api.WorkerRunLeaseClaimResponse{}, errors.New("Run Lease Environment ID is invalid")
		}
		materials, err := secretDelivery.OpenDeliveries(environmentID, envelopes)
		if err != nil {
			return api.WorkerRunLeaseClaimResponse{}, fmt.Errorf("open Run Lease Secret delivery: %w", err)
		}
		secrets, err = projectSecretDeliveries(materials)
		if err != nil {
			return api.WorkerRunLeaseClaimResponse{}, err
		}
	}
	return api.WorkerRunLeaseClaimResponse{
		Lease:     lease,
		Program:   program,
		Workspace: attachment,
		Secrets:   secrets,
		Execution: execution,
	}, nil
}

func deriveWorkspaceCapability(
	keys workspace.FencingKeys,
	lease db.WorkspaceLease,
) (workspace.FencingCapability, error) {
	fingerprint, err := workspace.FencingKeyFingerprintFromBytes(
		lease.FencingKeyFingerprint,
	)
	if err != nil {
		return workspace.FencingCapability{}, err
	}
	leaseID, err := pgvalue.UUIDValue(lease.ID)
	if err != nil {
		return workspace.FencingCapability{}, errors.New("Workspace Lease ID is invalid")
	}
	workspaceID, err := pgvalue.UUIDValue(lease.WorkspaceID)
	if err != nil {
		return workspace.FencingCapability{}, errors.New("Workspace ID is invalid")
	}
	capability, err := keys.Derive(fingerprint, workspace.FenceInput{
		LeaseID:                leaseID,
		WorkspaceID:            workspaceID,
		OwnershipGeneration:    lease.OwnershipGeneration,
		WriterGeneration:       lease.WriterGeneration,
		MountFencingGeneration: lease.MountFencingGeneration,
	})
	if err != nil {
		return workspace.FencingCapability{}, err
	}
	if subtle.ConstantTimeCompare(
		[]byte(capability.Hash),
		[]byte(lease.FencingTokenHash),
	) != 1 {
		return workspace.FencingCapability{}, errors.New("Workspace write capability does not match its Lease")
	}
	return capability, nil
}
