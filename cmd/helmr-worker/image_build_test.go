package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerImageControlMapsTerminalAdmissionWithoutHiddenState(t *testing.T) {
	request := testWorkerImageAdmission()
	attemptID := uuid.Must(uuid.NewV7()).String()
	result := imagebuild.GuestResult{
		ExecutionABI: imagebuild.ExecutionABI, Outcome: imagebuild.GuestSucceeded,
		OCIDigest: testWorkerImageDigest("a"), OCISizeBytes: 42,
	}
	fake := &workerImageControlFake{admit: func(input workerapi.WorkspaceImageAdmissionRequest) workerapi.WorkspaceImageAssignment {
		if input.Lease != workerImageAPILease(request.Lease) || input.DeclarationSlot != request.DeclarationSlot {
			t.Fatalf("admission input = %#v", input)
		}
		return workerapi.WorkspaceImageAssignment{
			Lease: input.Lease, DeclarationSlot: input.DeclarationSlot,
			OperationID: uuid.Must(uuid.NewV7()).String(), RequestFingerprint: testWorkerImageDigest("b"),
			RuntimeIdentityID: input.RuntimeIdentityID, Architecture: input.Architecture,
			Plan: input.Plan, PlanDigest: request.PlanDigest,
			SubmittedSourceDigest: input.SubmittedSourceDigest,
			BuildTreeDigest:       input.BuildTreeDigest, BuildTreeSizeBytes: input.BuildTreeSizeBytes,
			AdmittedPaths: input.AdmittedPaths, AdmittedPathSetDigest: request.AdmittedPathSetDigest,
			SourceArchiveDigest:    input.SourceArchiveDigest,
			SourceArchiveSizeBytes: input.SourceArchiveSizeBytes,
			SourceArchiveEntries:   input.SourceArchiveEntries,
			RequestedCacheMode:     request.RequestedCacheMode,
			CacheScope:             testWorkerImageDigest("c"), ExecutionABI: request.ExecutionABI,
			LLBABI: request.LLBABI, CacheABI: request.CacheABI,
			Quotas: workerapi.WorkspaceImageQuotas{
				CPUMillis: 3000, MemoryBytes: 4 << 30, ScratchBytes: 32 << 30, PIDs: 1024,
				MaxSourceArchiveBytes:   imagebuild.MaxSourceArchiveBytes,
				MaxSourceArchiveEntries: imagebuild.MaxSourceArchiveEntries,
				MaxOCIArchiveBytes:      imagebuild.MaxOCIArchiveBytes,
			},
			Output: workerapi.WorkspaceImageOutputContract{
				Architecture: request.Architecture, MediaType: "application/vnd.helmr.workspace-image.v0.oci-tar",
				MaxSizeBytes: imagebuild.MaxOCIArchiveBytes,
			},
			RegistryBindings:    []imagebuild.RegistryBinding{},
			ResolutionSetDigest: imagebuild.ResolutionSetDigest([]imagebuild.RegistryBinding{}),
			TerminalResult:      &workerapi.WorkspaceImageTerminalResult{AttemptID: attemptID, Result: result},
		}
	}}
	assignment, err := (workerImageControl{client: fake}).AdmitWorkspaceImage(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(assignment.Request, request) || assignment.TerminalResult == nil ||
		assignment.TerminalResult.Evidence.AttemptID != attemptID ||
		assignment.TerminalResult.Evidence.Lease != request.Lease ||
		assignment.TerminalResult.Evidence.GuestResult != result {
		t.Fatalf("assignment = %#v", assignment)
	}
}

func TestWorkerImageControlClearsMismatchedCredentialResponse(t *testing.T) {
	password := []byte("plaintext")
	lease := testWorkerImageLease()
	fake := &workerImageControlFake{credentials: workerapi.WorkspaceImageCredentialResponse{
		Envelope: imagebuild.CredentialEnvelope{
			OperationID: "wrong", AttemptID: uuid.Must(uuid.NewV7()).String(),
			ResolutionSetDigest: testWorkerImageDigest("d"),
			RegistryCredentials: []imagebuild.RegistryCredentialValue{{
				Authority: "docker.io", Username: "user", Password: password,
			}},
		},
	}}
	_, err := (workerImageControl{client: fake}).FetchRegistryCredentials(t.Context(), imagebuild.RegistryCredentialRequest{
		OperationID: uuid.Must(uuid.NewV7()).String(), AttemptID: uuid.Must(uuid.NewV7()).String(),
		Lease: lease, RegistryBindings: []imagebuild.RegistryBinding{},
		PlanDigest:          testWorkerImageDigest("e"),
		ResolutionSetDigest: imagebuild.ResolutionSetDigest([]imagebuild.RegistryBinding{}),
	})
	if err == nil {
		t.Fatal("mismatched credential response was accepted")
	}
	for _, value := range password {
		if value != 0 {
			t.Fatal("mismatched credential plaintext was not cleared")
		}
	}
}

func TestWorkerImageControlCompletesExactReceipt(t *testing.T) {
	lease := testWorkerImageLease()
	evidence := imagebuild.WorkerResultEvidence{
		OperationID: uuid.Must(uuid.NewV7()).String(), RequestFingerprint: testWorkerImageDigest("1"),
		AttemptID: uuid.Must(uuid.NewV7()).String(), Lease: lease, DeclarationSlot: "workspace",
		PlanDigest: testWorkerImageDigest("2"), ResolutionSetDigest: testWorkerImageDigest("3"),
		RequestedCacheMode: imagebuild.CachePrefer,
		GuestResult: imagebuild.GuestResult{
			ExecutionABI: imagebuild.ExecutionABI, Outcome: imagebuild.GuestSucceeded,
			OCIDigest: testWorkerImageDigest("4"), OCISizeBytes: 42,
		},
	}
	fake := &workerImageControlFake{complete: func(request workerapi.WorkspaceImageOperationResultRequest) workerapi.WorkspaceImageOperationResultResponse {
		if request.Lease != workerImageAPILease(lease) || request.DeclarationSlot != evidence.DeclarationSlot ||
			request.AttemptID != evidence.AttemptID || request.Result != evidence.GuestResult {
			t.Fatalf("completion input = %#v", request)
		}
		return workerapi.WorkspaceImageOperationResultResponse{
			OperationID: request.OperationID, AttemptID: request.AttemptID,
			State: "completed", Result: request.Result,
		}
	}}
	err := (workerImageControl{client: fake}).CompleteWorkspaceImage(t.Context(), imagebuild.CompletionRequest{
		Evidence: evidence,
		Artifact: &imagebuild.PublishedArtifact{
			Digest: evidence.GuestResult.OCIDigest, SizeBytes: evidence.GuestResult.OCISizeBytes,
			MediaType: "application/vnd.helmr.workspace-image.v0.oci-tar",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkerImageCacheCredentialTransfersPlaintextOwnership(t *testing.T) {
	password := []byte("cache-token")
	provider := &workerImageCacheFake{credential: imagecache.Credential{
		Authority: "registry.example", Username: "AWS", Password: password,
	}}
	value, err := (workerImageCacheCredentials{provider: provider}).FetchImageCacheCredential(
		t.Context(),
		imagebuild.Assignment{CacheBinding: &imagebuild.CacheBinding{
			Authority: "registry.example", Username: "AWS", Ref: "registry.example/cache:ref",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Password) != "cache-token" || provider.target.Ref != "registry.example/cache:ref" {
		t.Fatalf("credential = %#v target = %#v", value, provider.target)
	}
}

type workerImageControlFake struct {
	admit       func(workerapi.WorkspaceImageAdmissionRequest) workerapi.WorkspaceImageAssignment
	credentials workerapi.WorkspaceImageCredentialResponse
	complete    func(workerapi.WorkspaceImageOperationResultRequest) workerapi.WorkspaceImageOperationResultResponse
}

func (fake *workerImageControlFake) AdmitWorkspaceImage(_ context.Context, request workerapi.WorkspaceImageAdmissionRequest) (workerapi.WorkspaceImageAssignment, error) {
	return fake.admit(request), nil
}

func (fake *workerImageControlFake) FetchWorkspaceImageCredentials(context.Context, workerapi.WorkspaceImageCredentialRequest) (workerapi.WorkspaceImageCredentialResponse, error) {
	return fake.credentials, nil
}

func (fake *workerImageControlFake) CompleteWorkspaceImage(_ context.Context, request workerapi.WorkspaceImageOperationResultRequest) (workerapi.WorkspaceImageOperationResultResponse, error) {
	return fake.complete(request), nil
}

type workerImageCacheFake struct {
	credential imagecache.Credential
	target     imagecache.Target
}

func (fake *workerImageCacheFake) Fetch(_ context.Context, target imagecache.Target) (imagecache.Credential, error) {
	fake.target = target
	if fake.credential.Password == nil {
		return imagecache.Credential{}, errors.New("missing credential")
	}
	return fake.credential, nil
}

func testWorkerImageAdmission() imagebuild.AdmissionRequest {
	return imagebuild.AdmissionRequest{
		Lease: testWorkerImageLease(), RuntimeIdentityID: testWorkerImageDigest("5"),
		DeclarationSlot: "workspace", Architecture: "x86_64",
		Plan: imagebuild.Build{}, PlanDigest: testWorkerImageDigest("6"),
		SubmittedSourceDigest: testWorkerImageDigest("7"), BuildTreeDigest: testWorkerImageDigest("8"),
		BuildTreeSizeBytes: 42, AdmittedPaths: []imagebuild.SourcePath{},
		AdmittedPathSetDigest: testWorkerImageDigest("9"),
		SourceArchiveDigest:   testWorkerImageDigest("a"), SourceArchiveSizeBytes: 42,
		SourceArchiveEntries: 0, RequestedCacheMode: imagebuild.CachePrefer,
		ExecutionABI: imagebuild.ExecutionABI, LLBABI: imagebuild.LLBABI, CacheABI: imagebuild.CacheABI,
	}
}

func testWorkerImageLease() imagebuild.BuildLeaseAuthority {
	return imagebuild.BuildLeaseAuthority{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
		ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
		DeploymentID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "build",
		WorkerInstanceID: uuid.Must(uuid.NewV7()).String(), WorkerEpoch: 1, Generation: 1,
		WorkerProtocolVersion: "test", RequestedGuestEphemeralDiskBytes: 32 << 30,
		RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30, RequestedBuildExecutors: 1,
	}
}

func testWorkerImageDigest(character string) string {
	var value strings.Builder
	for range 64 {
		value.WriteString(character)
	}
	return "sha256:" + value.String()
}
