package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestVMEngineExecutesExactOneShotImageBuild(t *testing.T) {
	plan := validBuild()
	plan.Images[0].Steps[0].From.Auth = &RegistryAuth{
		Username: "registry-user", PasswordSecret: "REGISTRY_TOKEN",
	}
	paths := []SourcePath{
		{Path: "package.json", Kind: SourcePathFile},
		{Path: "src", Kind: SourcePathDirectory},
	}
	sourceBytes := []byte("canonical-source-archive")
	source := &workerTestSource{
		body:  sourceBytes,
		paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest:    digestBytes(sourceBytes),
			ArchiveSizeBytes: int64(len(sourceBytes)),
			ArchiveEntries:   len(paths),
			PathSetDigest:    PathSetDigest(paths),
		},
	}
	operationID := uuid.Must(uuid.NewV7()).String()
	resolution := RegistryBinding{
		Authority: "docker.io", Username: "registry-user",
		ResolutionID:         uuid.Must(uuid.NewV7()).String(),
		SecretID:             uuid.Must(uuid.NewV7()).String(),
		SecretVersionID:      uuid.Must(uuid.NewV7()).String(),
		RevocationGeneration: 1,
	}
	var assignmentRequest AdmissionRequest
	admission := workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
		assignmentRequest = request
		assignment := validWorkerAssignment(request, operationID)
		assignment.RegistryBindings = []RegistryBinding{resolution}
		assignment.ResolutionSetDigest = ResolutionSetDigest([]RegistryBinding{resolution})
		assignment.CacheBinding = &CacheBinding{
			Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
			Username:  "AWS",
			Ref:       "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
		}
		return assignment, nil
	}}
	userPassword := []byte("user-password")
	cachePassword := []byte("cache-password")
	order := make([]string, 0, 2)
	credentials := workerTestCredentials{fetch: func(request RegistryCredentialRequest) ([]RegistryCredentialValue, error) {
		order = append(order, "credentials")
		if request.OperationID != operationID || len(request.RegistryBindings) != 1 {
			t.Fatalf("credential request = %#v", request)
		}
		return []RegistryCredentialValue{{
			Authority: "docker.io", Username: "registry-user", Password: userPassword,
		}}, nil
	}}
	cache := workerTestCache{fetch: func(assignment Assignment) (RegistryCredentialValue, error) {
		order = append(order, "cache")
		if assignment.OperationID != operationID {
			t.Fatalf("cache assignment = %#v", assignment)
		}
		return RegistryCredentialValue{
			Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
			Username:  "AWS", Password: cachePassword,
		}, nil
	}}
	ociBytes := workerTestOCI(t)
	response := workerTestGuestResponse(t, ociBytes)
	stream := &workerTestStream{response: bytes.NewReader(response)}
	connector := &workerTestConnector{connect: func(request vm.ConnectRequest) vm.Session {
		order = append(order, "connect")
		if request.ID == "" || request.OwnerKind != vm.OwnerImageBuild ||
			request.Binding.OwnerID != request.ID || request.Binding.Generation != 1 ||
			request.Resources != compute.ImageBuildGuestResources() ||
			request.PIDsMax != compute.ImageBuildGuestPIDsMax {
			t.Fatalf("connect request = %#v", request)
		}
		stream.inspect = func(raw []byte) {
			guestRequest, envelope, archive := parseWorkerTestRequest(t, raw)
			if guestRequest.OperationID != operationID || guestRequest.AttemptID != request.ID ||
				guestRequest.CacheBinding == nil || !bytes.Equal(archive, sourceBytes) {
				t.Fatalf("guest request = %#v archive=%q", guestRequest, archive)
			}
			if len(envelope.RegistryCredentials) != 2 ||
				envelope.RegistryCredentials[0].Authority != "123456789012.dkr.ecr.us-east-1.amazonaws.com" ||
				envelope.RegistryCredentials[1].Authority != "docker.io" {
				t.Fatalf("credential envelope = %#v", envelope)
			}
		}
		return &workerTestSession{stream: stream}
	}}
	revocations := &workerTestRevocations{}
	completion := &workerTestCompletion{}
	engine := VMEngine{
		Connector: connector, Admission: admission, Credentials: credentials,
		Cache: cache, Completion: completion, WorkDir: t.TempDir(),
	}
	request := validWorkerBuildRequest(t, plan, source)
	request.RequestedCacheMode = CachePrefer
	artifact, err := engine.BuildWorkspaceImage(t.Context(), request, revocations)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"connect", "credentials", "cache"}) {
		t.Fatalf("execution order = %v", order)
	}
	if revocations.operationID != operationID || !revocations.unregistered {
		t.Fatalf("revocation registration = %#v", revocations)
	}
	if assignmentRequest.ExecutionABI != ExecutionABI ||
		assignmentRequest.SourceArchiveDigest != source.descriptor.ArchiveDigest {
		t.Fatalf("admission request = %#v", assignmentRequest)
	}
	if !bytes.Equal(userPassword, make([]byte, len(userPassword))) ||
		!bytes.Equal(cachePassword, make([]byte, len(cachePassword))) {
		t.Fatal("credential buffers were not cleared")
	}
	if artifact.Digest != digestBytes(ociBytes) || artifact.SizeBytes != int64(len(ociBytes)) {
		t.Fatalf("artifact = %#v", artifact)
	}
	if err := engine.CompleteWorkspaceImage(t.Context(), artifact, PublishedArtifact{
		Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: "application/vnd.helmr.workspace-image.v0+oci",
	}); err != nil {
		t.Fatal(err)
	}
	if len(completion.requests) != 1 || completion.requests[0].Evidence.OperationID != operationID {
		t.Fatalf("completion requests = %#v", completion.requests)
	}
	file, err := artifact.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, ociBytes) {
		t.Fatalf("artifact bytes mismatch read=%v close=%v", readErr, closeErr)
	}
	path := artifact.path
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact remains after close: %v", err)
	}
}

func TestVMEngineRejectsAssignmentThatChangesAdmittedFactsBeforeVM(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	source := &workerTestSource{
		body: []byte("source"), paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes([]byte("source")), ArchiveSizeBytes: 6,
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	connected := false
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			connected = true
			return nil
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			changed := request
			changed.BuildTreeDigest = digestBytes([]byte("different"))
			return validWorkerAssignment(changed, uuid.Must(uuid.NewV7()).String()), nil
		}},
		Credentials: workerTestCredentials{}, Completion: &workerTestCompletion{}, WorkDir: t.TempDir(),
	}
	if _, err := engine.BuildWorkspaceImage(t.Context(), validWorkerBuildRequest(t, plan, source), &workerTestRevocations{}); err == nil {
		t.Fatal("mismatched assignment was accepted")
	}
	if connected {
		t.Fatal("VM was connected before assignment exact-match")
	}
}

func TestVMEngineReplaysTerminalSuccessWithoutVMOrCredentials(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	sourceBody := []byte("source")
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	image := workerTestOCI(t)
	operationID := uuid.Must(uuid.NewV7()).String()
	completion := &workerTestCompletion{}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			t.Fatal("terminal replay connected a VM")
			return nil
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			assignment := validWorkerAssignment(request, operationID)
			attemptID := uuid.Must(uuid.NewV7()).String()
			result := GuestResult{
				ExecutionABI: ExecutionABI, Outcome: GuestSucceeded,
				OCIDigest: digestBytes(image), OCISizeBytes: int64(len(image)),
			}
			assignment.TerminalResult = &TerminalResult{
				Evidence: resultEvidence(assignment, attemptID, result),
			}
			return assignment, nil
		}},
		Credentials: workerTestCredentials{fetch: func(RegistryCredentialRequest) ([]RegistryCredentialValue, error) {
			t.Fatal("terminal replay fetched credentials")
			return nil, nil
		}},
		Completion: completion,
		WorkDir:    t.TempDir(),
	}
	revocations := &workerTestRevocations{}
	artifact, err := engine.BuildWorkspaceImage(t.Context(), validWorkerBuildRequest(t, plan, source), revocations)
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.Replayed || artifact.Digest != digestBytes(image) || artifact.Evidence.AttemptID == "" {
		t.Fatalf("replayed artifact = %#v", artifact)
	}
	if revocations.operationID != "" {
		t.Fatal("terminal replay registered a physical attempt")
	}
	if _, err := artifact.Open(); err == nil {
		t.Fatal("terminal replay exposed a local artifact")
	}
	if err := engine.CompleteWorkspaceImage(t.Context(), artifact, PublishedArtifact{
		Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: "application/vnd.helmr.workspace-image.v0+oci",
	}); err != nil {
		t.Fatal(err)
	}
	if len(completion.requests) != 0 {
		t.Fatalf("terminal replay completed an already-terminal claim: %#v", completion.requests)
	}
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVMEngineRevocationCancelsAndDestroysOnlyLiveAttempt(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	sourceBody := []byte("source")
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	operationID := uuid.Must(uuid.NewV7()).String()
	stream := &workerTestStream{response: bytes.NewReader(nil)}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			return &workerTestSession{stream: stream}
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			return validWorkerAssignment(request, operationID), nil
		}},
		Credentials: blockingWorkerTestCredentials{}, Completion: &workerTestCompletion{},
		WorkDir: t.TempDir(),
	}
	registered := make(chan context.CancelFunc, 1)
	revocations := cancelingWorkerTestRevocations{registered: registered}
	done := make(chan error, 1)
	go func() {
		_, err := engine.BuildWorkspaceImage(context.Background(), validWorkerBuildRequest(t, plan, source), revocations)
		done <- err
	}()
	var cancel context.CancelFunc
	select {
	case cancel = <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("image operation was not registered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("revoked build error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked image attempt did not stop")
	}
	if !stream.closed {
		t.Fatal("revoked image attempt did not destroy its VM session")
	}
}

func TestVMEngineCompletesGuestFailureBeforeReturningIt(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	sourceBody := []byte("source")
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	operationID := uuid.Must(uuid.NewV7()).String()
	failureRaw, err := CanonicalGuestResult(GuestResult{
		ExecutionABI: ExecutionABI, Outcome: GuestFailed,
		FailureReason: GuestFailureImage, Error: "build failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	var response bytes.Buffer
	if err := frameio.WriteMessageFrame(&response, failureRaw); err != nil {
		t.Fatal(err)
	}
	completion := &workerTestCompletion{}
	session := &workerTestSession{stream: &workerTestStream{response: bytes.NewReader(response.Bytes())}}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			return session
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			return validWorkerAssignment(request, operationID), nil
		}},
		Credentials: workerTestCredentials{}, Completion: completion, WorkDir: t.TempDir(),
	}
	_, err = engine.BuildWorkspaceImage(t.Context(), validWorkerBuildRequest(t, plan, source), &workerTestRevocations{})
	var guestFailure *WorkerGuestFailure
	if !errors.As(err, &guestFailure) || guestFailure.Message != "build failed" {
		t.Fatalf("guest failure = %v", err)
	}
	if len(completion.requests) != 1 || completion.requests[0].Artifact != nil ||
		completion.requests[0].Evidence.OperationID != operationID ||
		completion.requests[0].Evidence.AttemptID == "" ||
		completion.requests[0].Evidence.GuestResult.Outcome != GuestFailed {
		t.Fatalf("failure completion = %#v", completion.requests)
	}
	if session.statusCalls != 1 {
		t.Fatalf("network status calls = %d, want 1", session.statusCalls)
	}
}

func TestVMEngineNetworkQuotaOverridesGuestFailure(t *testing.T) {
	plan := validBuild()
	sourceBody := []byte("source")
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	failureRaw, err := CanonicalGuestResult(GuestResult{
		ExecutionABI: ExecutionABI, Outcome: GuestFailed,
		FailureReason: GuestFailureImage, Error: "ordinary image failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	var response bytes.Buffer
	if err := frameio.WriteMessageFrame(&response, failureRaw); err != nil {
		t.Fatal(err)
	}
	session := &workerTestSession{
		stream:        &workerTestStream{response: bytes.NewReader(response.Bytes())},
		networkStatus: vm.BuildNetworkStatus{LimitPackets: 1},
	}
	completion := &workerTestCompletion{}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session { return session }},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			return validWorkerAssignment(request, uuid.Must(uuid.NewV7()).String()), nil
		}},
		Credentials: workerTestCredentials{}, Completion: completion, WorkDir: t.TempDir(),
	}
	_, err = engine.BuildWorkspaceImage(t.Context(), validWorkerBuildRequest(t, plan, source), &workerTestRevocations{})
	var guestFailure *WorkerGuestFailure
	if !errors.As(err, &guestFailure) || guestFailure.Reason != GuestFailureNetworkQuota {
		t.Fatalf("network quota failure = %v", err)
	}
	if session.statusCalls != 1 || len(completion.requests) != 1 ||
		completion.requests[0].Evidence.GuestResult.FailureReason != GuestFailureNetworkQuota {
		t.Fatalf("network quota completion = %#v calls=%d", completion.requests, session.statusCalls)
	}
}

func TestVMEngineNetworkQuotaOverridesSuccessfulArtifact(t *testing.T) {
	plan := validBuild()
	sourceBody := []byte("source")
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	ociBytes := workerTestOCI(t)
	session := &workerTestSession{
		stream:        &workerTestStream{response: bytes.NewReader(workerTestGuestResponse(t, ociBytes))},
		networkStatus: vm.BuildNetworkStatus{LimitPackets: 1},
	}
	completion := &workerTestCompletion{}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session { return session }},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			return validWorkerAssignment(request, uuid.Must(uuid.NewV7()).String()), nil
		}},
		Credentials: workerTestCredentials{}, Completion: completion, WorkDir: t.TempDir(),
	}
	artifact, err := engine.BuildWorkspaceImage(t.Context(), validWorkerBuildRequest(t, plan, source), &workerTestRevocations{})
	var guestFailure *WorkerGuestFailure
	if artifact != nil || !errors.As(err, &guestFailure) || guestFailure.Reason != GuestFailureNetworkQuota {
		t.Fatalf("network quota result artifact=%#v err=%v", artifact, err)
	}
	if session.statusCalls != 1 || len(completion.requests) != 1 || completion.requests[0].Artifact != nil ||
		completion.requests[0].Evidence.GuestResult.FailureReason != GuestFailureNetworkQuota {
		t.Fatalf("network quota completion = %#v calls=%d", completion.requests, session.statusCalls)
	}
	entries, err := os.ReadDir(engine.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("network-limited artifact was retained: %v", entries)
	}
}

func TestVMEngineRunsColdWhenCacheCredentialIsUnavailableBeforeDelivery(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	sourceBody := []byte("source")
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	operationID := uuid.Must(uuid.NewV7()).String()
	ociBytes := workerTestOCI(t)
	stream := &workerTestStream{response: bytes.NewReader(workerTestGuestResponse(t, ociBytes))}
	stream.inspect = func(raw []byte) {
		request, envelope, _ := parseWorkerTestRequest(t, raw)
		if request.RequestedCacheMode != CachePrefer || request.CacheBinding != nil {
			t.Fatalf("cold guest request = %#v", request)
		}
		if len(envelope.RegistryCredentials) != 0 {
			t.Fatalf("cold credential envelope = %#v", envelope)
		}
	}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			return &workerTestSession{stream: stream}
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			assignment := validWorkerAssignment(request, operationID)
			assignment.CacheBinding = &CacheBinding{
				Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com", Username: "AWS",
				Ref: "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
			}
			return assignment, nil
		}},
		Credentials: workerTestCredentials{},
		Cache: workerTestCache{fetch: func(Assignment) (RegistryCredentialValue, error) {
			return RegistryCredentialValue{}, &imagecache.UnavailableError{
				Operation: "token", Err: errors.New("throttled"),
			}
		}},
		Completion: &workerTestCompletion{}, WorkDir: t.TempDir(),
	}
	request := validWorkerBuildRequest(t, plan, source)
	request.RequestedCacheMode = CachePrefer
	artifact, err := engine.BuildWorkspaceImage(t.Context(), request, &workerTestRevocations{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if artifact.Evidence.RequestedCacheMode != CachePrefer {
		t.Fatalf("requested cache mode = %q", artifact.Evidence.RequestedCacheMode)
	}
}

func TestVMEngineRejectsCacheCredentialContractFailureBeforeDelivery(t *testing.T) {
	plan := validBuild()
	paths := []SourcePath{{Path: "package.json", Kind: SourcePathFile}, {Path: "src", Kind: SourcePathDirectory}}
	sourceBody := []byte("source")
	source := &workerTestSource{
		body: sourceBody, paths: paths,
		descriptor: SourceArchiveDescriptor{
			ArchiveDigest: digestBytes(sourceBody), ArchiveSizeBytes: int64(len(sourceBody)),
			ArchiveEntries: len(paths), PathSetDigest: PathSetDigest(paths),
		},
	}
	stream := &workerTestStream{response: bytes.NewReader(nil)}
	engine := VMEngine{
		Connector: &workerTestConnector{connect: func(vm.ConnectRequest) vm.Session {
			return &workerTestSession{stream: stream}
		}},
		Admission: workerTestAdmission{admit: func(request AdmissionRequest) (Assignment, error) {
			assignment := validWorkerAssignment(request, uuid.Must(uuid.NewV7()).String())
			assignment.CacheBinding = &CacheBinding{
				Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com", Username: "AWS",
				Ref: "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
			}
			return assignment, nil
		}},
		Credentials: workerTestCredentials{},
		Cache: workerTestCache{fetch: func(Assignment) (RegistryCredentialValue, error) {
			return RegistryCredentialValue{}, &imagecache.ContractError{Message: "target mismatch"}
		}},
		Completion: &workerTestCompletion{}, WorkDir: t.TempDir(),
	}
	request := validWorkerBuildRequest(t, plan, source)
	request.RequestedCacheMode = CachePrefer
	if _, err := engine.BuildWorkspaceImage(t.Context(), request, &workerTestRevocations{}); err == nil {
		t.Fatal("cache contract failure was accepted")
	}
	if stream.request.Len() != 0 {
		t.Fatal("guest request was delivered after cache contract failure")
	}
}

type workerTestSource struct {
	body       []byte
	paths      []SourcePath
	descriptor SourceArchiveDescriptor
}

func (source *workerTestSource) Descriptor() (SourceArchiveDescriptor, error) {
	return source.descriptor, nil
}

func (source *workerTestSource) Paths() ([]SourcePath, error) {
	return slices.Clone(source.paths), nil
}

func (source *workerTestSource) WriteTo(_ context.Context, writer io.Writer) error {
	_, err := writer.Write(source.body)
	return err
}

type workerTestAdmission struct {
	admit func(AdmissionRequest) (Assignment, error)
}

func (client workerTestAdmission) AdmitWorkspaceImage(_ context.Context, request AdmissionRequest) (Assignment, error) {
	return client.admit(request)
}

type workerTestCredentials struct {
	fetch func(RegistryCredentialRequest) ([]RegistryCredentialValue, error)
}

type blockingWorkerTestCredentials struct{}

func (blockingWorkerTestCredentials) FetchRegistryCredentials(ctx context.Context, _ RegistryCredentialRequest) ([]RegistryCredentialValue, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (fetcher workerTestCredentials) FetchRegistryCredentials(_ context.Context, request RegistryCredentialRequest) ([]RegistryCredentialValue, error) {
	if fetcher.fetch == nil {
		return []RegistryCredentialValue{}, nil
	}
	return fetcher.fetch(request)
}

type workerTestCache struct {
	fetch func(Assignment) (RegistryCredentialValue, error)
}

type workerTestCompletion struct {
	requests []CompletionRequest
}

func (client *workerTestCompletion) CompleteWorkspaceImage(_ context.Context, request CompletionRequest) error {
	client.requests = append(client.requests, request)
	return nil
}

func (provider workerTestCache) FetchImageCacheCredential(_ context.Context, assignment Assignment) (RegistryCredentialValue, error) {
	return provider.fetch(assignment)
}

type workerTestRevocations struct {
	operationID  string
	cancel       context.CancelFunc
	unregistered bool
}

type cancelingWorkerTestRevocations struct {
	registered chan<- context.CancelFunc
}

func (registry cancelingWorkerTestRevocations) RegisterImageOperation(_ string, cancel context.CancelFunc) (func(), error) {
	registry.registered <- cancel
	return func() {}, nil
}

func (registry *workerTestRevocations) RegisterImageOperation(operationID string, cancel context.CancelFunc) (func(), error) {
	registry.operationID = operationID
	registry.cancel = cancel
	return func() { registry.unregistered = true }, nil
}

type workerTestConnector struct {
	connect func(vm.ConnectRequest) vm.Session
}

func (connector *workerTestConnector) Connect(_ context.Context, request vm.ConnectRequest) (vm.Session, error) {
	return connector.connect(request), nil
}

type workerTestSession struct {
	stream        *workerTestStream
	networkStatus vm.BuildNetworkStatus
	networkErr    error
	statusCalls   int
}

func (session *workerTestSession) Stream() vm.Stream { return session.stream }
func (*workerTestSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("unsupported")
}
func (*workerTestSession) Wait(context.Context) error { return nil }
func (session *workerTestSession) Close(context.Context) error {
	session.stream.closed = true
	return nil
}
func (session *workerTestSession) BuildNetworkStatus(context.Context) (vm.BuildNetworkStatus, error) {
	session.statusCalls++
	return session.networkStatus, session.networkErr
}

type workerTestStream struct {
	request  bytes.Buffer
	response *bytes.Reader
	inspect  func([]byte)
	closed   bool
}

func (stream *workerTestStream) Read(body []byte) (int, error)  { return stream.response.Read(body) }
func (stream *workerTestStream) Write(body []byte) (int, error) { return stream.request.Write(body) }
func (stream *workerTestStream) Close() error                   { stream.closed = true; return nil }
func (stream *workerTestStream) CloseWrite() error {
	if stream.inspect != nil {
		stream.inspect(stream.request.Bytes())
	}
	return nil
}

func validWorkerBuildRequest(t *testing.T, plan Build, source SourceArchive) WorkerBuildRequest {
	t.Helper()
	return WorkerBuildRequest{
		Lease: BuildLeaseAuthority{
			ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
			ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
			DeploymentID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "build",
			WorkerInstanceID: uuid.Must(uuid.NewV7()).String(), WorkerEpoch: 1, Generation: 1,
			WorkerProtocolVersion: "test", RequestedGuestEphemeralDiskBytes: 32 << 30,
			RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30, RequestedBuildExecutors: 1,
		},
		RuntimeIdentityID: digestBytes([]byte("runtime")),
		DeclarationSlot:   "workspace", Architecture: "x86_64", Plan: plan,
		SubmittedSourceDigest: digestBytes([]byte("submitted")),
		BuildTreeDigest:       digestBytes([]byte("tree")), BuildTreeSizeBytes: 2048,
		RequestedCacheMode: CacheBypass, Source: source,
	}
}

func validWorkerAssignment(request AdmissionRequest, operationID string) Assignment {
	resources := compute.ImageBuildGuestResources()
	return Assignment{
		OperationID: operationID, RequestFingerprint: digestBytes([]byte("request")), Request: request,
		RegistryBindings: []RegistryBinding{}, ResolutionSetDigest: ResolutionSetDigest([]RegistryBinding{}),
		CacheScope: digestBytes([]byte("cache-scope")),
		Quotas: AssignmentQuotas{
			CPUMillis: resources.MilliCPU, MemoryBytes: resources.MemoryMiB << 20,
			ScratchBytes: resources.DiskMiB << 20, PIDs: compute.ImageBuildGuestPIDsMax,
			MaxSourceArchiveBytes:   MaxSourceArchiveBytes,
			MaxSourceArchiveEntries: MaxSourceArchiveEntries,
			MaxOCIArchiveBytes:      MaxOCIArchiveBytes,
		},
		Output: AssignmentOutputContract{
			Architecture: request.Architecture,
			MediaType:    "application/vnd.helmr.workspace-image.v0+oci",
			MaxSizeBytes: MaxOCIArchiveBytes,
		},
	}
}

func parseWorkerTestRequest(t *testing.T, raw []byte) (GuestRequest, CredentialEnvelope, []byte) {
	t.Helper()
	reader := bytes.NewReader(raw)
	header, bodySize, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeImageBuild || bodySize != uint64(reader.Len()) {
		t.Fatalf("wire header = %#v size=%d remaining=%d", header, bodySize, reader.Len())
	}
	requestRaw, err := frameio.ReadMessageFrameBounded(reader, RequestDocumentMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ParseGuestRequest(requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	archive := make([]byte, request.SourceArchiveSizeBytes)
	if _, err := io.ReadFull(reader, archive); err != nil {
		t.Fatal(err)
	}
	envelopeRaw, err := frameio.ReadMessageFrameBounded(reader, CredentialEnvelopeMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ParseCredentialEnvelope(envelopeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Len() != 0 {
		t.Fatalf("request retains %d bytes", reader.Len())
	}
	return request, envelope, archive
}

func workerTestGuestResponse(t *testing.T, image []byte) []byte {
	t.Helper()
	raw, err := CanonicalGuestResult(GuestResult{
		ExecutionABI: ExecutionABI, Outcome: GuestSucceeded,
		OCIDigest: digestBytes(image), OCISizeBytes: int64(len(image)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var response bytes.Buffer
	if err := frameio.WriteMessageFrame(&response, raw); err != nil {
		t.Fatal(err)
	}
	response.Write(image)
	return response.Bytes()
}

func workerTestOCI(t *testing.T) []byte {
	t.Helper()
	config := []byte(`{"Config":{"WorkingDir":"/workspace"}}`)
	configDigest := digestBytes(config)
	manifest, err := json.Marshal(oci.Manifest{
		Config: oci.Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest, Size: int64(len(config)),
		},
		Layers: []oci.Descriptor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digestBytes(manifest)
	index, err := json.Marshal(oci.Index{Manifests: []oci.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    manifestDigest, Size: int64(len(manifest)),
		Platform: &oci.Platform{Architecture: "amd64", OS: "linux"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for name, content := range map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": index,
		"blobs/sha256/" + strings.TrimPrefix(configDigest, "sha256:"):   config,
		"blobs/sha256/" + strings.TrimPrefix(manifestDigest, "sha256:"): manifest,
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}
