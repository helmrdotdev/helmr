package controlplane

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runLeaseClaimProjection struct {
	program     db.GetDeploymentProgramAuthorityRow
	definition  db.DeploymentDefinition
	resetTarget db.GetComputerDiskVersionAuthorityRow
}

type runLeaseClaimResponseAuthority struct {
	session  db.Session
	run      db.Run
	attempt  db.RunAttempt
	instance db.ComputerInstance
	runLease db.RunLease
	computer db.LockRunLeaseClaimComputerRow
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
		return runLeaseClaimProjection{}, fmt.Errorf("load run lease program authority: %w", err)
	}
	definition, err := store.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: authority.run.EnvironmentID,
		DeploymentID:  authority.run.DeploymentID,
		Kind:          authority.run.EntrypointKind,
		DeclaredID:    authority.run.EntrypointDeclaredID,
	})
	if err != nil {
		return runLeaseClaimProjection{}, fmt.Errorf("load run lease declaration authority: %w", err)
	}
	projection := runLeaseClaimProjection{
		program:    program,
		definition: definition,
	}
	projection.resetTarget, err = store.GetComputerDiskVersionAuthority(
		ctx,
		db.GetComputerDiskVersionAuthorityParams{
			OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
			EnvironmentID: authority.run.EnvironmentID, ComputerID: authority.computer.ID,
			VersionID: authority.attempt.BaseComputerDiskVersionID,
		},
	)
	if err != nil {
		return runLeaseClaimProjection{}, fmt.Errorf("load run lease computer reset target authority: %w", err)
	}
	return projection, nil
}

func projectRunLeaseClaimResponse(
	ctx context.Context,
	authority runLeaseClaimResponseAuthority,
	envelopes []secret.DeliveryEnvelope,
	projection runLeaseClaimProjection,
	platformStore cas.Reader,
	secretDelivery SecretDeliveryOpener,
	fencingKey disk.FencingKey,
) (workerapi.RunLeaseClaimResponse, error) {
	physical := runLeaseProjectionAuthority{
		run:      authority.run,
		attempt:  authority.attempt,
		instance: authority.instance,
		runLease: authority.runLease,
		computer: authority.computer,
	}
	lease, err := projectRunLeaseAssignment(physical)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	program, err := projectDeploymentProgram(ctx, projection.program, platformStore)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	if projection.program.DeploymentID != authority.run.DeploymentID ||
		projection.program.EnvironmentID != authority.run.EnvironmentID {
		return workerapi.RunLeaseClaimResponse{}, errors.New("run lease program authority is inconsistent")
	}
	var session *db.Session
	if authority.session.ID.Valid {
		session = &authority.session
	}
	start, err := encodeProgramStart(authority.run, authority.attempt, session, projection.definition, projection.program.DeploymentVersion)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	capability, err := computer.WriteCapability(fencingKey, authority.instance)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	attachment, err := projectComputerAttachment(physical, capability.Token, projection.resetTarget)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	if secretDelivery == nil {
		return workerapi.RunLeaseClaimResponse{}, errors.New("secret delivery opener is not configured")
	}
	environmentID, err := pgvalue.UUIDValue(authority.run.EnvironmentID)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, errors.New("run lease environment ID is invalid")
	}
	materials, err := secretDelivery.OpenDeliveries(environmentID, envelopes)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, fmt.Errorf("open run lease secret delivery: %w", err)
	}
	secrets, err := projectSecretDeliveries(materials)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	return workerapi.RunLeaseClaimResponse{
		Lease:        lease,
		Program:      program,
		Computer:     attachment,
		Secrets:      secrets,
		ProgramStart: start,
	}, nil
}

func projectRestoredRunLeaseClaim(claim run.Claim, key disk.FencingKey) (workerapi.RunLeaseClaimResponse, error) {
	wait, _ := claim.ResumeWait()
	r, instance := claim.Run(), claim.Instance()
	lease, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{run: r, attempt: claim.Attempt(), instance: instance, runLease: claim.Lease(), computer: claim.Computer()})
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	capability, err := computer.WriteCapability(key, instance)
	if err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	return workerapi.RunLeaseClaimResponse{Lease: lease, Computer: workerapi.ComputerAttachment{WriteCapability: capability.Token, Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: lease.BaseComputerDiskVersionID}}, ProgramResume: &workerapi.ProgramResume{CheckpointID: pgvalue.UUIDString(instance.SourceCheckpointID), RunWaitID: pgvalue.UUIDString(wait.ID), EntrypointKind: r.EntrypointKind}}, nil
}
