package imagebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const imageBuildCloseTimeout = 30 * time.Second

// SourceArchive is the sealed post-lifecycle source selection admitted for one
// Workspace image. WriteTo must reproduce the descriptor and path set returned
// by Descriptor and Paths or fail without selecting another source.
type SourceArchive interface {
	Descriptor() (SourceArchiveDescriptor, error)
	Paths() ([]SourcePath, error)
	WriteTo(context.Context, io.Writer) error
}

type SourceArchiveDescriptor struct {
	ArchiveDigest    string
	ArchiveSizeBytes int64
	ArchiveEntries   int
	PathSetDigest    string
}

// BuildLeaseAuthority is the provider-neutral Control authority required by
// every Workspace-image admission, credential delivery, and completion call.
// It deliberately mirrors the complete fenced assignment rather than keeping
// an operation-to-Lease lookup in the Worker.
type BuildLeaseAuthority struct {
	ID                               string
	OrgID                            string
	ProjectID                        string
	EnvironmentID                    string
	DeploymentID                     string
	WorkerGroupID                    string
	WorkerInstanceID                 string
	WorkerEpoch                      int64
	Generation                       int64
	WorkerProtocolVersion            string
	RequestedGuestEphemeralDiskBytes int64
	RequestedCPUMillis               int64
	RequestedMemoryBytes             int64
	RequestedBuildExecutors          int32
}

// WorkerBuildRequest contains the exact Worker facts submitted to Control for
// Workspace-image admission. It contains no credential plaintext.
type WorkerBuildRequest struct {
	Lease                 BuildLeaseAuthority
	RuntimeIdentityID     string
	DeclarationSlot       string
	Architecture          string
	Plan                  Build
	SubmittedSourceDigest string
	BuildTreeDigest       string
	BuildTreeSizeBytes    int64
	RequestedCacheMode    CacheMode
	Source                SourceArchive
}

type AdmissionRequest struct {
	Lease                  BuildLeaseAuthority
	RuntimeIdentityID      string
	DeclarationSlot        string
	Architecture           string
	Plan                   Build
	PlanDigest             string
	SubmittedSourceDigest  string
	BuildTreeDigest        string
	BuildTreeSizeBytes     int64
	AdmittedPaths          []SourcePath
	AdmittedPathSetDigest  string
	SourceArchiveDigest    string
	SourceArchiveSizeBytes int64
	SourceArchiveEntries   int
	RequestedCacheMode     CacheMode
	ExecutionABI           string
	LLBABI                 string
	CacheABI               string
}

// Assignment is Control's non-secret, current-Build-Lease dispatch result.
// Request must exactly echo the admitted Worker facts.
type Assignment struct {
	OperationID         string
	RequestFingerprint  string
	Request             AdmissionRequest
	RegistryBindings    []RegistryBinding
	ResolutionSetDigest string
	CacheScope          string
	Quotas              AssignmentQuotas
	Output              AssignmentOutputContract
	CacheBinding        *CacheBinding
	TerminalResult      *TerminalResult
}

type TerminalResult struct {
	Evidence WorkerResultEvidence
}

type AssignmentQuotas struct {
	CPUMillis               int64
	MemoryBytes             int64
	ScratchBytes            int64
	PIDs                    int64
	MaxSourceArchiveBytes   int64
	MaxSourceArchiveEntries int
	MaxOCIArchiveBytes      int64
}

type AssignmentOutputContract struct {
	Architecture string
	MediaType    string
	MaxSizeBytes int64
}

type AdmissionClient interface {
	AdmitWorkspaceImage(context.Context, AdmissionRequest) (Assignment, error)
}

type RegistryCredentialRequest struct {
	OperationID         string
	AttemptID           string
	Lease               BuildLeaseAuthority
	RegistryBindings    []RegistryBinding
	PlanDigest          string
	ResolutionSetDigest string
}

type RegistryCredentialFetcher interface {
	FetchRegistryCredentials(context.Context, RegistryCredentialRequest) ([]RegistryCredentialValue, error)
}

type PublishedArtifact struct {
	Digest    string
	SizeBytes int64
	MediaType string
}

type CompletionRequest struct {
	Evidence WorkerResultEvidence
	Artifact *PublishedArtifact
}

type CompletionClient interface {
	CompleteWorkspaceImage(context.Context, CompletionRequest) error
}

// CacheCredentialFetcher is called only after a guest is connected and only
// when Control supplied a non-secret attempt-local cache binding.
type CacheCredentialFetcher interface {
	FetchImageCacheCredential(context.Context, Assignment) (RegistryCredentialValue, error)
}

type OperationRevocations interface {
	RegisterImageOperation(string, context.CancelFunc) (func(), error)
}

type WorkerImageBuilder interface {
	BuildWorkspaceImage(context.Context, WorkerBuildRequest, OperationRevocations) (*WorkerArtifact, error)
	CompleteWorkspaceImage(context.Context, *WorkerArtifact, PublishedArtifact) error
}

// WorkerArtifact owns one verified temporary OCI archive. Callers may open it
// repeatedly for CAS publication and must Close it to remove the local copy.
type WorkerArtifact struct {
	Digest    string
	SizeBytes int64
	Evidence  WorkerResultEvidence
	Replayed  bool
	path      string
	closed    bool
}

// WorkerResultEvidence is returned with the verified temporary OCI archive so
// the caller can publish to CAS before completing the logical Control claim.
type WorkerResultEvidence struct {
	OperationID            string
	RequestFingerprint     string
	AttemptID              string
	Lease                  BuildLeaseAuthority
	DeclarationSlot        string
	RuntimeIdentityID      string
	PlanDigest             string
	SubmittedSourceDigest  string
	BuildTreeDigest        string
	AdmittedPathSetDigest  string
	SourceArchiveDigest    string
	SourceArchiveSizeBytes int64
	ResolutionSetDigest    string
	CacheScope             string
	RequestedCacheMode     CacheMode
	Output                 AssignmentOutputContract
	GuestResult            GuestResult
}

func (artifact *WorkerArtifact) Open() (*os.File, error) {
	if artifact == nil || artifact.closed || artifact.path == "" {
		return nil, errors.New("Workspace image artifact is closed")
	}
	return os.Open(artifact.path)
}

func (artifact *WorkerArtifact) Close() error {
	if artifact == nil || artifact.closed {
		return nil
	}
	if artifact.Replayed {
		artifact.closed = true
		return nil
	}
	if err := os.Remove(artifact.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	artifact.closed = true
	artifact.path = ""
	return nil
}

type VMEngine struct {
	Connector   vm.Connector
	Admission   AdmissionClient
	Credentials RegistryCredentialFetcher
	Cache       CacheCredentialFetcher
	Completion  CompletionClient
	WorkDir     string
}

func (engine VMEngine) BuildWorkspaceImage(
	ctx context.Context,
	request WorkerBuildRequest,
	revocations OperationRevocations,
) (_ *WorkerArtifact, returnErr error) {
	if ctx == nil {
		return nil, errors.New("Workspace image build context is nil")
	}
	if engine.Connector == nil || engine.Admission == nil || engine.Credentials == nil || engine.Completion == nil {
		return nil, errors.New("Workspace image VM engine dependencies are incomplete")
	}
	if engine.WorkDir == "" || !filepath.IsAbs(engine.WorkDir) || filepath.Clean(engine.WorkDir) != engine.WorkDir {
		return nil, errors.New("Workspace image VM work directory must be absolute and clean")
	}
	if revocations == nil {
		return nil, errors.New("Workspace image operation revocations are required")
	}

	admissionRequest, err := prepareAdmissionRequest(request)
	if err != nil {
		return nil, err
	}
	assignment, err := engine.Admission.AdmitWorkspaceImage(ctx, admissionRequest)
	if err != nil {
		return nil, fmt.Errorf("admit Workspace image: %w", err)
	}
	if err := validateAssignment(admissionRequest, assignment); err != nil {
		return nil, err
	}
	if assignment.TerminalResult != nil {
		evidence := assignment.TerminalResult.Evidence
		if evidence.GuestResult.Outcome == GuestFailed {
			return nil, &WorkerGuestFailure{
				Reason:  evidence.GuestResult.FailureReason,
				Message: evidence.GuestResult.Error,
			}
		}
		return &WorkerArtifact{
			Digest: evidence.GuestResult.OCIDigest, SizeBytes: evidence.GuestResult.OCISizeBytes,
			Evidence: evidence, Replayed: true,
		}, nil
	}

	attemptID := uuid.Must(uuid.NewV7()).String()
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	unregister, err := revocations.RegisterImageOperation(assignment.OperationID, cancelAttempt)
	if err != nil {
		return nil, err
	}
	defer unregister()

	owner := vm.Owner{Kind: vm.OwnerImageBuild, ID: attemptID}
	session, err := engine.Connector.Connect(attemptCtx, vm.ConnectRequest{
		ID:        attemptID,
		OwnerKind: vm.OwnerImageBuild,
		Binding: vm.WorkloadBinding{
			WorkerEpoch:       assignment.Request.Lease.WorkerEpoch,
			OwnerID:           attemptID,
			Generation:        1,
			RuntimeIdentityID: assignment.Request.RuntimeIdentityID,
		},
		Resources: compute.ImageBuildGuestResources(),
		PIDsMax:   compute.ImageBuildGuestPIDsMax,
	})
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("connect image-build guest: %w", err))
	}
	closeDone := make(chan error, 1)
	stopCancellationClose := context.AfterFunc(attemptCtx, func() {
		closeDone <- closeImageBuildSession(session, owner)
	})
	var produced *WorkerArtifact
	defer func() {
		unregister()
		var closeErr error
		if stopCancellationClose() {
			closeErr = closeImageBuildSession(session, owner)
		} else {
			closeErr = <-closeDone
		}
		returnErr = errors.Join(returnErr, closeErr)
		if returnErr != nil && produced != nil {
			returnErr = errors.Join(returnErr, produced.Close())
		}
	}()
	network, ok := session.(vm.BuildNetworkSession)
	if !ok {
		return nil, errors.New("image-build session does not expose network status")
	}
	stream := session.Stream()
	if stream == nil {
		return nil, errors.New("image-build session stream is unavailable")
	}

	userCredentials, err := engine.Credentials.FetchRegistryCredentials(
		attemptCtx,
		RegistryCredentialRequest{
			OperationID:         assignment.OperationID,
			AttemptID:           attemptID,
			Lease:               assignment.Request.Lease,
			RegistryBindings:    slices.Clone(assignment.RegistryBindings),
			PlanDigest:          assignment.Request.PlanDigest,
			ResolutionSetDigest: assignment.ResolutionSetDigest,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("fetch Workspace image registry credentials: %w", err)
	}
	defer clearCredentialValues(userCredentials)
	credentials := make([]RegistryCredentialValue, len(userCredentials), len(userCredentials)+1)
	copy(credentials, userCredentials)
	if assignment.CacheBinding != nil {
		if engine.Cache == nil {
			return nil, errors.New("Workspace image cache credential fetcher is required by assignment")
		}
		cacheCredential, err := engine.Cache.FetchImageCacheCredential(attemptCtx, assignment)
		if err != nil {
			if !imagecache.IsUnavailable(err) {
				return nil, fmt.Errorf("fetch Workspace image cache credential: %w", err)
			}
			// Cache availability is not execution authority. A typed provider
			// outage before request delivery removes the attempt-local binding
			// while preserving the immutable requested prefer mode.
			assignment.CacheBinding = nil
		} else {
			defer clear(cacheCredential.Password)
			credentials = append(credentials, cacheCredential)
		}
	}
	guestRequest := assignmentGuestRequest(assignment, attemptID)
	requestRaw, err := CanonicalGuestRequest(guestRequest)
	if err != nil {
		return nil, fmt.Errorf("encode Workspace image guest request: %w", err)
	}
	slices.SortFunc(credentials, func(left, right RegistryCredentialValue) int {
		return strings.Compare(left.Authority, right.Authority)
	})
	envelope := CredentialEnvelope{
		OperationID:         assignment.OperationID,
		AttemptID:           attemptID,
		ResolutionSetDigest: assignment.ResolutionSetDigest,
		RegistryCredentials: credentials,
	}
	envelopeRaw, err := CanonicalCredentialEnvelope(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode Workspace image credential envelope: %w", err)
	}
	defer clear(envelopeRaw)
	if err := MatchCredentialEnvelope(guestRequest, envelope); err != nil {
		return nil, err
	}

	bodySize := uint64(4+len(requestRaw)) + uint64(admissionRequest.SourceArchiveSizeBytes) + uint64(4+len(envelopeRaw))
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeImageBuild,
		OperationID: assignment.OperationID,
	}, bodySize); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write image-build header: %w", err))
	}
	if err := frameio.WriteMessageFrame(stream, requestRaw); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write image-build request: %w", err))
	}
	counter := &boundedCountingWriter{
		writer:    stream,
		remaining: admissionRequest.SourceArchiveSizeBytes,
	}
	if err := request.Source.WriteTo(attemptCtx, counter); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write image-build source: %w", err))
	}
	if counter.remaining != 0 {
		return nil, vm.NewGuestError(errors.New("image-build source size changed after admission"))
	}
	if err := frameio.WriteMessageFrame(stream, envelopeRaw); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write image-build credentials: %w", err))
	}
	clear(envelopeRaw)
	clearCredentialValues(credentials)
	if err := stream.CloseWrite(); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("half-close image-build request: %w", err))
	}

	resultRaw, err := frameio.ReadMessageFrameBounded(stream, RequestDocumentMaxBytes)
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("read image-build result: %w", err))
	}
	result, err := ParseGuestResult(resultRaw)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	if result.Outcome == GuestFailed {
		if err := requireResponseEOF(stream); err != nil {
			return nil, vm.NewGuestError(err)
		}
		status, err := network.BuildNetworkStatus(attemptCtx)
		if err != nil {
			return nil, fmt.Errorf("read image-build network status: %w", err)
		}
		if status.LimitPackets != 0 {
			result = imageBuildNetworkQuotaResult()
		}
		if err := engine.Completion.CompleteWorkspaceImage(attemptCtx, CompletionRequest{
			Evidence: resultEvidence(assignment, attemptID, result),
		}); err != nil {
			return nil, fmt.Errorf("complete failed Workspace image operation: %w", err)
		}
		return nil, &WorkerGuestFailure{Reason: result.FailureReason, Message: result.Error}
	}

	artifact, err := receiveWorkerArtifact(attemptCtx, engine.WorkDir, stream, result, admissionRequest.Architecture)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	produced = artifact
	artifact.Evidence = resultEvidence(assignment, attemptID, result)
	status, err := network.BuildNetworkStatus(attemptCtx)
	if err != nil {
		return nil, fmt.Errorf("read image-build network status: %w", err)
	}
	if status.LimitPackets != 0 {
		result = imageBuildNetworkQuotaResult()
		if err := engine.Completion.CompleteWorkspaceImage(attemptCtx, CompletionRequest{
			Evidence: resultEvidence(assignment, attemptID, result),
		}); err != nil {
			return nil, fmt.Errorf("complete network-limited Workspace image operation: %w", err)
		}
		return nil, &WorkerGuestFailure{Reason: result.FailureReason, Message: result.Error}
	}
	return artifact, nil
}

func imageBuildNetworkQuotaResult() GuestResult {
	return GuestResult{
		ExecutionABI:  ExecutionABI,
		Outcome:       GuestFailed,
		FailureReason: GuestFailureNetworkQuota,
		Error:         "image-build public-egress limit was exceeded",
	}
}

func resultEvidence(assignment Assignment, attemptID string, result GuestResult) WorkerResultEvidence {
	return WorkerResultEvidence{
		OperationID:            assignment.OperationID,
		RequestFingerprint:     assignment.RequestFingerprint,
		AttemptID:              attemptID,
		Lease:                  assignment.Request.Lease,
		DeclarationSlot:        assignment.Request.DeclarationSlot,
		RuntimeIdentityID:      assignment.Request.RuntimeIdentityID,
		PlanDigest:             assignment.Request.PlanDigest,
		SubmittedSourceDigest:  assignment.Request.SubmittedSourceDigest,
		BuildTreeDigest:        assignment.Request.BuildTreeDigest,
		AdmittedPathSetDigest:  assignment.Request.AdmittedPathSetDigest,
		SourceArchiveDigest:    assignment.Request.SourceArchiveDigest,
		SourceArchiveSizeBytes: assignment.Request.SourceArchiveSizeBytes,
		ResolutionSetDigest:    assignment.ResolutionSetDigest,
		CacheScope:             assignment.CacheScope,
		RequestedCacheMode:     assignment.Request.RequestedCacheMode,
		Output:                 assignment.Output,
		GuestResult:            result,
	}
}

func (engine VMEngine) CompleteWorkspaceImage(
	ctx context.Context,
	artifact *WorkerArtifact,
	published PublishedArtifact,
) error {
	if artifact == nil {
		return errors.New("Workspace image completion artifact is required")
	}
	if artifact.closed {
		return errors.New("Workspace image completion artifact is closed")
	}
	if artifact.Replayed {
		return nil
	}
	if engine.Completion == nil {
		return errors.New("Workspace image completion client is required")
	}
	if artifact.Digest != published.Digest || artifact.SizeBytes != published.SizeBytes ||
		published.MediaType == "" || published.MediaType != artifact.Evidence.Output.MediaType ||
		artifact.Evidence.GuestResult.Outcome != GuestSucceeded ||
		artifact.Evidence.GuestResult.OCIDigest != published.Digest ||
		artifact.Evidence.GuestResult.OCISizeBytes != published.SizeBytes {
		return errors.New("Workspace image completion does not match the verified artifact")
	}
	return engine.Completion.CompleteWorkspaceImage(ctx, CompletionRequest{
		Evidence: artifact.Evidence,
		Artifact: &published,
	})
}

type WorkerGuestFailure struct {
	Reason  GuestFailureReason
	Message string
}

func (failure *WorkerGuestFailure) Error() string {
	if failure == nil {
		return "Workspace image guest failed"
	}
	return failure.Message
}

func prepareAdmissionRequest(request WorkerBuildRequest) (AdmissionRequest, error) {
	if request.Source == nil {
		return AdmissionRequest{}, errors.New("Workspace image source is required")
	}
	descriptor, err := request.Source.Descriptor()
	if err != nil {
		return AdmissionRequest{}, err
	}
	paths, err := request.Source.Paths()
	if err != nil {
		return AdmissionRequest{}, err
	}
	planDigest, err := Digest(request.Plan, request.Architecture)
	if err != nil {
		return AdmissionRequest{}, err
	}
	admission := AdmissionRequest{
		Lease:                  request.Lease,
		RuntimeIdentityID:      request.RuntimeIdentityID,
		DeclarationSlot:        request.DeclarationSlot,
		Architecture:           request.Architecture,
		Plan:                   request.Plan,
		PlanDigest:             planDigest,
		SubmittedSourceDigest:  request.SubmittedSourceDigest,
		BuildTreeDigest:        request.BuildTreeDigest,
		BuildTreeSizeBytes:     request.BuildTreeSizeBytes,
		AdmittedPaths:          paths,
		AdmittedPathSetDigest:  descriptor.PathSetDigest,
		SourceArchiveDigest:    descriptor.ArchiveDigest,
		SourceArchiveSizeBytes: descriptor.ArchiveSizeBytes,
		SourceArchiveEntries:   descriptor.ArchiveEntries,
		RequestedCacheMode:     request.RequestedCacheMode,
		ExecutionABI:           ExecutionABI,
		LLBABI:                 LLBABI,
		CacheABI:               CacheABI,
	}
	if err := validateAdmissionRequest(admission); err != nil {
		return AdmissionRequest{}, err
	}
	return admission, nil
}

func validateAdmissionRequest(request AdmissionRequest) error {
	if err := validateBuildLeaseAuthority(request.Lease); err != nil {
		return err
	}
	if !validDigest(request.RuntimeIdentityID) ||
		!validDigest(request.SubmittedSourceDigest) ||
		!validDigest(request.BuildTreeDigest) ||
		!validDigest(request.AdmittedPathSetDigest) ||
		!validDigest(request.SourceArchiveDigest) {
		return errors.New("Workspace image admission digest is invalid")
	}
	if request.DeclarationSlot == "" || len(request.DeclarationSlot) > 128 ||
		strings.TrimSpace(request.DeclarationSlot) != request.DeclarationSlot {
		return errors.New("Workspace image declaration slot is invalid")
	}
	if request.BuildTreeSizeBytes < 1 ||
		request.BuildTreeSizeBytes > MaxSourceArchiveBytes ||
		request.SourceArchiveSizeBytes < 1 || request.SourceArchiveSizeBytes > MaxSourceArchiveBytes ||
		request.SourceArchiveEntries != len(request.AdmittedPaths) ||
		request.SourceArchiveEntries > MaxSourceArchiveEntries {
		return errors.New("Workspace image admission size is invalid")
	}
	if request.ExecutionABI != ExecutionABI || request.LLBABI != LLBABI || request.CacheABI != CacheABI {
		return errors.New("Workspace image admission ABI is invalid")
	}
	if request.RequestedCacheMode != CachePrefer && request.RequestedCacheMode != CacheBypass {
		return errors.New("Workspace image requested cache mode is invalid")
	}
	if err := Validate(request.Plan, request.Architecture); err != nil {
		return err
	}
	digest, err := Digest(request.Plan, request.Architecture)
	if err != nil || digest != request.PlanDigest {
		return errors.New("Workspace image admission plan digest is invalid")
	}
	if request.AdmittedPaths == nil || !slices.IsSortedFunc(request.AdmittedPaths, func(left, right SourcePath) int {
		return strings.Compare(left.Path, right.Path)
	}) {
		return errors.New("Workspace image admitted paths are not sorted")
	}
	for index, admitted := range request.AdmittedPaths {
		if !validSourcePath(admitted) || index > 0 && request.AdmittedPaths[index-1].Path == admitted.Path {
			return errors.New("Workspace image admitted path is invalid")
		}
	}
	if PathSetDigest(request.AdmittedPaths) != request.AdmittedPathSetDigest {
		return errors.New("Workspace image admitted path-set digest is invalid")
	}
	return validateAdmittedPaths(request.Plan, request.AdmittedPaths)
}

func validateBuildLeaseAuthority(lease BuildLeaseAuthority) error {
	for _, value := range []string{
		lease.ID,
		lease.OrgID,
		lease.ProjectID,
		lease.EnvironmentID,
		lease.DeploymentID,
		lease.WorkerInstanceID,
	} {
		if ids.Validate(value) != nil {
			return errors.New("Workspace image Build Lease contains an invalid ID")
		}
	}
	if lease.WorkerGroupID == "" || strings.TrimSpace(lease.WorkerGroupID) != lease.WorkerGroupID ||
		lease.WorkerProtocolVersion == "" || strings.TrimSpace(lease.WorkerProtocolVersion) != lease.WorkerProtocolVersion ||
		lease.WorkerEpoch < 1 || lease.Generation < 1 ||
		lease.RequestedGuestEphemeralDiskBytes < 1 || lease.RequestedCPUMillis < 1 ||
		lease.RequestedMemoryBytes < 1 || lease.RequestedBuildExecutors < 1 {
		return errors.New("Workspace image Build Lease fence is invalid")
	}
	return nil
}

func validateAssignment(request AdmissionRequest, assignment Assignment) error {
	if assignment.OperationID == "" || !validDigest(assignment.RequestFingerprint) ||
		!reflect.DeepEqual(request, assignment.Request) {
		return errors.New("Workspace image assignment does not exact-match admission")
	}
	guest := assignmentGuestRequest(assignment, uuid.Must(uuid.NewV7()).String())
	if err := ValidateGuestRequest(guest); err != nil {
		return fmt.Errorf("validate Workspace image assignment: %w", err)
	}
	resources := compute.ImageBuildGuestResources()
	expectedQuotas := AssignmentQuotas{
		CPUMillis: resources.MilliCPU, MemoryBytes: resources.MemoryMiB << 20,
		ScratchBytes: resources.DiskMiB << 20, PIDs: compute.ImageBuildGuestPIDsMax,
		MaxSourceArchiveBytes:   MaxSourceArchiveBytes,
		MaxSourceArchiveEntries: MaxSourceArchiveEntries,
		MaxOCIArchiveBytes:      MaxOCIArchiveBytes,
	}
	if !validDigest(assignment.CacheScope) || assignment.Quotas != expectedQuotas ||
		assignment.Output.Architecture != request.Architecture ||
		assignment.Output.MediaType == "" || assignment.Output.MaxSizeBytes != MaxOCIArchiveBytes {
		return errors.New("Workspace image assignment cache, quota, or output contract is invalid")
	}
	if assignment.TerminalResult != nil {
		terminal := assignment.TerminalResult.Evidence
		if err := ids.Validate(terminal.AttemptID); err != nil {
			return errors.New("Workspace image terminal attempt ID is invalid")
		}
		if err := ValidateGuestResult(terminal.GuestResult); err != nil {
			return fmt.Errorf("validate Workspace image terminal result: %w", err)
		}
		if expected := resultEvidence(assignment, terminal.AttemptID, terminal.GuestResult); !reflect.DeepEqual(expected, terminal) {
			return errors.New("Workspace image terminal receipt does not exact-match assignment")
		}
	}
	return nil
}

func assignmentGuestRequest(assignment Assignment, attemptID string) GuestRequest {
	request := assignment.Request
	guest := GuestRequest{
		ExecutionABI: request.ExecutionABI, LLBABI: request.LLBABI, CacheABI: request.CacheABI,
		OperationID: assignment.OperationID, AttemptID: attemptID,
		BuildLeaseID: request.Lease.ID, BuildLeaseGeneration: request.Lease.Generation,
		WorkerEpoch: request.Lease.WorkerEpoch, RuntimeIdentityID: request.RuntimeIdentityID,
		Architecture: request.Architecture, Plan: request.Plan, PlanDigest: request.PlanDigest,
		SubmittedSourceDigest: request.SubmittedSourceDigest, BuildTreeDigest: request.BuildTreeDigest,
		AdmittedPaths: slices.Clone(request.AdmittedPaths), AdmittedPathSetDigest: request.AdmittedPathSetDigest,
		SourceArchiveDigest: request.SourceArchiveDigest, SourceArchiveSizeBytes: request.SourceArchiveSizeBytes,
		SourceArchiveEntries: request.SourceArchiveEntries, ResolutionSetDigest: assignment.ResolutionSetDigest,
		RegistryBindings: slices.Clone(assignment.RegistryBindings), RequestedCacheMode: request.RequestedCacheMode,
	}
	if assignment.CacheBinding != nil {
		binding := *assignment.CacheBinding
		guest.CacheBinding = &binding
	}
	return guest
}

func receiveWorkerArtifact(
	ctx context.Context,
	workDir string,
	stream io.Reader,
	result GuestResult,
	architecture string,
) (_ *WorkerArtifact, returnErr error) {
	file, err := os.CreateTemp(workDir, ".helmr-image-build-")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer func() {
		closeErr := file.Close()
		returnErr = errors.Join(returnErr, closeErr)
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(path))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(file, hash), &contextReader{ctx: ctx, reader: stream}, result.OCISizeBytes)
	if err != nil || written != result.OCISizeBytes {
		return nil, fmt.Errorf("read image-build OCI: %w", err)
	}
	if err := requireResponseEOF(stream); err != nil {
		return nil, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if digest != result.OCIDigest {
		return nil, errors.New("image-build OCI digest does not match result")
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	metadata, err := oci.Inspect(file)
	if err != nil {
		return nil, fmt.Errorf("inspect image-build OCI: %w", err)
	}
	if metadata.ManifestCount != 1 || metadata.Platform == nil || metadata.Platform.OS != "linux" ||
		architecture != "x86_64" || metadata.Platform.Architecture != "amd64" {
		return nil, errors.New("image-build OCI platform does not match linux/amd64")
	}
	return &WorkerArtifact{Digest: digest, SizeBytes: written, path: path}, nil
}

func closeImageBuildSession(session vm.Session, owner vm.Owner) error {
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildCloseTimeout)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		return &vm.CleanupUnprovenError{Owner: owner, Cause: err}
	}
	return nil
}

func clearCredentialValues(credentials []RegistryCredentialValue) {
	for index := range credentials {
		clear(credentials[index].Password)
	}
}

func requireResponseEOF(reader io.Reader) error {
	var trailing [1]byte
	count, err := reader.Read(trailing[:])
	if count != 0 || !errors.Is(err, io.EOF) {
		return errors.New("image-build response contains trailing data")
	}
	return nil
}

type boundedCountingWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *boundedCountingWriter) Write(body []byte) (int, error) {
	if int64(len(body)) > writer.remaining {
		return 0, errors.New("image-build source exceeds its admitted size")
	}
	written, err := writer.writer.Write(body)
	writer.remaining -= int64(written)
	return written, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}

var _ WorkerImageBuilder = VMEngine{}
