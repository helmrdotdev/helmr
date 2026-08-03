package main

import (
	"context"
	"errors"
	"slices"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type workerImageControlClient interface {
	AdmitWorkspaceImage(context.Context, workerapi.WorkspaceImageAdmissionRequest) (workerapi.WorkspaceImageAssignment, error)
	FetchWorkspaceImageCredentials(context.Context, workerapi.WorkspaceImageCredentialRequest) (workerapi.WorkspaceImageCredentialResponse, error)
	CompleteWorkspaceImage(context.Context, workerapi.WorkspaceImageOperationResultRequest) (workerapi.WorkspaceImageOperationResultResponse, error)
}

type workerImageControl struct {
	client workerImageControlClient
}

func (control workerImageControl) AdmitWorkspaceImage(
	ctx context.Context,
	request imagebuild.AdmissionRequest,
) (imagebuild.Assignment, error) {
	if control.client == nil {
		return imagebuild.Assignment{}, errors.New("Workspace image Control client is required")
	}
	response, err := control.client.AdmitWorkspaceImage(ctx, workerapi.WorkspaceImageAdmissionRequest{
		Lease:                  workerImageAPILease(request.Lease),
		DeclarationSlot:        request.DeclarationSlot,
		RuntimeIdentityID:      request.RuntimeIdentityID,
		Architecture:           request.Architecture,
		Plan:                   request.Plan,
		SubmittedSourceDigest:  request.SubmittedSourceDigest,
		BuildTreeDigest:        request.BuildTreeDigest,
		BuildTreeSizeBytes:     request.BuildTreeSizeBytes,
		AdmittedPaths:          slices.Clone(request.AdmittedPaths),
		SourceArchiveDigest:    request.SourceArchiveDigest,
		SourceArchiveSizeBytes: request.SourceArchiveSizeBytes,
		SourceArchiveEntries:   request.SourceArchiveEntries,
	})
	if err != nil {
		return imagebuild.Assignment{}, err
	}
	assignment := imagebuild.Assignment{
		OperationID:        response.OperationID,
		RequestFingerprint: response.RequestFingerprint,
		Request: imagebuild.AdmissionRequest{
			Lease:                  workerImageLeaseAuthority(response.Lease),
			RuntimeIdentityID:      response.RuntimeIdentityID,
			DeclarationSlot:        response.DeclarationSlot,
			Architecture:           response.Architecture,
			Plan:                   response.Plan,
			PlanDigest:             response.PlanDigest,
			SubmittedSourceDigest:  response.SubmittedSourceDigest,
			BuildTreeDigest:        response.BuildTreeDigest,
			BuildTreeSizeBytes:     response.BuildTreeSizeBytes,
			AdmittedPaths:          slices.Clone(response.AdmittedPaths),
			AdmittedPathSetDigest:  response.AdmittedPathSetDigest,
			SourceArchiveDigest:    response.SourceArchiveDigest,
			SourceArchiveSizeBytes: response.SourceArchiveSizeBytes,
			SourceArchiveEntries:   response.SourceArchiveEntries,
			RequestedCacheMode:     response.RequestedCacheMode,
			ExecutionABI:           response.ExecutionABI,
			LLBABI:                 response.LLBABI,
			CacheABI:               response.CacheABI,
		},
		RegistryBindings:    slices.Clone(response.RegistryBindings),
		ResolutionSetDigest: response.ResolutionSetDigest,
		CacheScope:          response.CacheScope,
		Quotas: imagebuild.AssignmentQuotas{
			CPUMillis: response.Quotas.CPUMillis, MemoryBytes: response.Quotas.MemoryBytes,
			ScratchBytes: response.Quotas.ScratchBytes, PIDs: response.Quotas.PIDs,
			MaxSourceArchiveBytes:   response.Quotas.MaxSourceArchiveBytes,
			MaxSourceArchiveEntries: response.Quotas.MaxSourceArchiveEntries,
			MaxOCIArchiveBytes:      response.Quotas.MaxOCIArchiveBytes,
		},
		Output: imagebuild.AssignmentOutputContract{
			Architecture: response.Output.Architecture,
			MediaType:    response.Output.MediaType,
			MaxSizeBytes: response.Output.MaxSizeBytes,
		},
	}
	if response.CacheTarget != nil {
		binding := response.CacheTarget.Binding
		assignment.CacheBinding = &binding
	}
	if response.TerminalResult != nil {
		assignment.TerminalResult = &imagebuild.TerminalResult{Evidence: workerImageResultEvidence(
			assignment,
			response.TerminalResult.AttemptID,
			response.TerminalResult.Result,
		)}
	}
	return assignment, nil
}

func (control workerImageControl) FetchRegistryCredentials(
	ctx context.Context,
	request imagebuild.RegistryCredentialRequest,
) ([]imagebuild.RegistryCredentialValue, error) {
	if control.client == nil {
		return nil, errors.New("Workspace image Control client is required")
	}
	response, err := control.client.FetchWorkspaceImageCredentials(ctx, workerapi.WorkspaceImageCredentialRequest{
		Lease: workerImageAPILease(request.Lease), OperationID: request.OperationID,
		AttemptID: request.AttemptID, PlanDigest: request.PlanDigest,
		ResolutionSetDigest: request.ResolutionSetDigest,
	})
	if err != nil {
		return nil, err
	}
	credentials := response.Envelope.RegistryCredentials
	if response.Envelope.OperationID != request.OperationID ||
		response.Envelope.AttemptID != request.AttemptID ||
		response.Envelope.ResolutionSetDigest != request.ResolutionSetDigest ||
		imagebuild.ResolutionSetDigest(request.RegistryBindings) != request.ResolutionSetDigest {
		clearWorkerImageCredentials(credentials)
		return nil, errors.New("Workspace image credential response does not exact-match the request")
	}
	expected := make(map[string]string, len(request.RegistryBindings))
	for _, binding := range request.RegistryBindings {
		expected[binding.Authority] = binding.Username
	}
	if len(credentials) != len(expected) {
		clearWorkerImageCredentials(credentials)
		return nil, errors.New("Workspace image credential response is incomplete")
	}
	for _, credential := range credentials {
		if expected[credential.Authority] != credential.Username || len(credential.Password) == 0 {
			clearWorkerImageCredentials(credentials)
			return nil, errors.New("Workspace image credential authority does not exact-match the request")
		}
		delete(expected, credential.Authority)
	}
	if len(expected) != 0 {
		clearWorkerImageCredentials(credentials)
		return nil, errors.New("Workspace image credential response is incomplete")
	}
	return credentials, nil
}

func (control workerImageControl) CompleteWorkspaceImage(
	ctx context.Context,
	request imagebuild.CompletionRequest,
) error {
	if control.client == nil {
		return errors.New("Workspace image Control client is required")
	}
	if request.Evidence.GuestResult.Outcome == imagebuild.GuestSucceeded {
		if request.Artifact == nil || request.Artifact.Digest != request.Evidence.GuestResult.OCIDigest ||
			request.Artifact.SizeBytes != request.Evidence.GuestResult.OCISizeBytes {
			return errors.New("successful Workspace image completion is missing its published Artifact")
		}
	} else if request.Artifact != nil {
		return errors.New("failed Workspace image completion must not contain an Artifact")
	}
	response, err := control.client.CompleteWorkspaceImage(ctx, workerapi.WorkspaceImageOperationResultRequest{
		Lease:           workerImageAPILease(request.Evidence.Lease),
		DeclarationSlot: request.Evidence.DeclarationSlot,
		OperationID:     request.Evidence.OperationID, AttemptID: request.Evidence.AttemptID,
		RequestFingerprint: request.Evidence.RequestFingerprint,
		PlanDigest:         request.Evidence.PlanDigest, ResolutionSetDigest: request.Evidence.ResolutionSetDigest,
		RequestedCacheMode: request.Evidence.RequestedCacheMode, Result: request.Evidence.GuestResult,
	})
	if err != nil {
		return err
	}
	wantState := "completed"
	if request.Evidence.GuestResult.Outcome == imagebuild.GuestFailed {
		wantState = "failed"
	}
	if response.OperationID != request.Evidence.OperationID || response.AttemptID != request.Evidence.AttemptID ||
		response.State != wantState || response.Result != request.Evidence.GuestResult {
		return errors.New("Workspace image completion response does not exact-match the terminal receipt")
	}
	return nil
}

type workerImageCacheCredentials struct {
	provider imagecache.CredentialProvider
}

func (provider workerImageCacheCredentials) FetchImageCacheCredential(
	ctx context.Context,
	assignment imagebuild.Assignment,
) (imagebuild.RegistryCredentialValue, error) {
	if provider.provider == nil || assignment.CacheBinding == nil {
		return imagebuild.RegistryCredentialValue{}, &imagecache.ContractError{Message: "cache credential provider or binding is absent"}
	}
	binding := assignment.CacheBinding
	credential, err := provider.provider.Fetch(ctx, imagecache.Target{
		Authority: binding.Authority, Username: binding.Username, Ref: binding.Ref,
	})
	if err != nil {
		return imagebuild.RegistryCredentialValue{}, err
	}
	value := imagebuild.RegistryCredentialValue{
		Authority: credential.Authority, Username: credential.Username, Password: credential.Password,
	}
	credential.Password = nil
	return value, nil
}

func workerImageAPILease(lease imagebuild.BuildLeaseAuthority) workerapi.DeploymentBuildLease {
	return workerapi.DeploymentBuildLease{
		ID: lease.ID, OrgID: lease.OrgID, ProjectID: lease.ProjectID,
		EnvironmentID: lease.EnvironmentID, DeploymentID: lease.DeploymentID,
		WorkerGroupID: lease.WorkerGroupID, WorkerInstanceID: lease.WorkerInstanceID,
		WorkerEpoch: lease.WorkerEpoch, LeaseSequence: lease.Generation,
		WorkerProtocolVersion:            lease.WorkerProtocolVersion,
		RequestedGuestEphemeralDiskBytes: lease.RequestedGuestEphemeralDiskBytes,
		RequestedCPUMillis:               lease.RequestedCPUMillis, RequestedMemoryBytes: lease.RequestedMemoryBytes,
		RequestedBuildExecutors: lease.RequestedBuildExecutors,
	}
}

func workerImageLeaseAuthority(lease workerapi.DeploymentBuildLease) imagebuild.BuildLeaseAuthority {
	return imagebuild.BuildLeaseAuthority{
		ID: lease.ID, OrgID: lease.OrgID, ProjectID: lease.ProjectID,
		EnvironmentID: lease.EnvironmentID, DeploymentID: lease.DeploymentID,
		WorkerGroupID: lease.WorkerGroupID, WorkerInstanceID: lease.WorkerInstanceID,
		WorkerEpoch: lease.WorkerEpoch, Generation: lease.LeaseSequence,
		WorkerProtocolVersion:            lease.WorkerProtocolVersion,
		RequestedGuestEphemeralDiskBytes: lease.RequestedGuestEphemeralDiskBytes,
		RequestedCPUMillis:               lease.RequestedCPUMillis, RequestedMemoryBytes: lease.RequestedMemoryBytes,
		RequestedBuildExecutors: lease.RequestedBuildExecutors,
	}
}

func workerImageResultEvidence(
	assignment imagebuild.Assignment,
	attemptID string,
	result imagebuild.GuestResult,
) imagebuild.WorkerResultEvidence {
	return imagebuild.WorkerResultEvidence{
		OperationID: assignment.OperationID, RequestFingerprint: assignment.RequestFingerprint,
		AttemptID: attemptID, Lease: assignment.Request.Lease,
		DeclarationSlot:   assignment.Request.DeclarationSlot,
		RuntimeIdentityID: assignment.Request.RuntimeIdentityID, PlanDigest: assignment.Request.PlanDigest,
		SubmittedSourceDigest:  assignment.Request.SubmittedSourceDigest,
		BuildTreeDigest:        assignment.Request.BuildTreeDigest,
		AdmittedPathSetDigest:  assignment.Request.AdmittedPathSetDigest,
		SourceArchiveDigest:    assignment.Request.SourceArchiveDigest,
		SourceArchiveSizeBytes: assignment.Request.SourceArchiveSizeBytes,
		ResolutionSetDigest:    assignment.ResolutionSetDigest, CacheScope: assignment.CacheScope,
		RequestedCacheMode: assignment.Request.RequestedCacheMode, Output: assignment.Output,
		GuestResult: result,
	}
}

func clearWorkerImageCredentials(credentials []imagebuild.RegistryCredentialValue) {
	for index := range credentials {
		for offset := range credentials[index].Password {
			credentials[index].Password[offset] = 0
		}
		credentials[index].Password = nil
	}
}

var (
	_ imagebuild.AdmissionClient           = workerImageControl{}
	_ imagebuild.RegistryCredentialFetcher = workerImageControl{}
	_ imagebuild.CompletionClient          = workerImageControl{}
	_ imagebuild.CacheCredentialFetcher    = workerImageCacheCredentials{}
	_ workerImageControlClient             = (*client.Client)(nil)
)
