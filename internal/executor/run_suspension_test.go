package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestCheckpointReadyRetryableStopsOnPermanentAdmissionFailure(t *testing.T) {
	if checkpointReadyRetryable(&httpclient.Error{StatusCode: http.StatusUnprocessableEntity}) {
		t.Fatal("unprocessable checkpoint admission remained retryable")
	}
	if !checkpointReadyRetryable(&httpclient.Error{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("transient checkpoint failure stopped retrying")
	}
}

func TestControlPlaneRunWaitsDetachesAfterTypedCheckpointIntent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 3, CheckpointID: "checkpoint-1",
		}},
	}
	checkpointer := &fakeCheckpointer{manifest: testRunCheckpointWaitManifest()}
	checkpointer.workspaceCapture = testCheckpointWorkspaceCapture()
	client.onReady = func() {
		if checkpointer.releaseCount != 0 {
			t.Fatalf("checkpoint source released before ready")
		}
		cancel()
	}

	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.ActiveDuration = 1500 * time.Millisecond
	request.Checkpointer = checkpointer
	err := ControlPlaneRunWaits{Client: client}.Wait(ctx, request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.RequestVersion != 3 || client.ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready request = %+v", client.ready)
	}
	if checkpointer.releaseCount != 1 {
		t.Fatalf("checkpoint source release count = %d, want 1", checkpointer.releaseCount)
	}
	if checkpointer.releaseContextErr != nil {
		t.Fatalf("checkpoint source release context = %v, want live cleanup context", checkpointer.releaseContextErr)
	}
	if checkpointer.request.CheckpointID != "checkpoint-1" ||
		checkpointer.request.RunID != "run-1" ||
		checkpointer.request.AttemptNumber != 2 ||
		checkpointer.request.RunLeaseID != "lease-1" ||
		checkpointer.request.ResumeAttachID != "resume-attach-1" ||
		checkpointer.request.CheckpointRequestVersion != 3 {
		t.Fatalf("checkpoint request = %+v", checkpointer.request)
	}
	if client.failed != nil {
		t.Fatalf("unexpected failed checkpoint = %+v", client.failed)
	}
}

func TestControlPlaneRunWaitsReleasesCheckpointSourceOnPermanentReadyFailure(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 3, CheckpointID: "checkpoint-1",
		}},
		readyErrors: []error{&httpclient.Error{StatusCode: http.StatusUnprocessableEntity}},
	}
	checkpointer := &fakeCheckpointer{
		manifest: testRunCheckpointWaitManifest(), workspaceCapture: testCheckpointWorkspaceCapture(),
	}
	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.Checkpointer = checkpointer

	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached after permanent ready failure", err)
	}
	if client.failed == nil || !strings.Contains(client.failed.Error, "mark checkpoint ready") {
		t.Fatalf("failed request = %+v, want permanent ready failure", client.failed)
	}
	if checkpointer.releaseCount != 1 {
		t.Fatalf("checkpoint source release count = %d, want 1", checkpointer.releaseCount)
	}
}

func TestControlPlaneRunWaitsPreservesReadyAndFailureAdmissionErrors(t *testing.T) {
	readyErr := &httpclient.Error{StatusCode: http.StatusUnprocessableEntity, Message: "ready rejected"}
	failureErr := &httpclient.Error{StatusCode: http.StatusConflict, Message: "failure stale"}
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 3, CheckpointID: "checkpoint-1",
		}},
		readyErrors:             []error{readyErr},
		checkpointFailureErrors: []error{failureErr},
	}
	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.Checkpointer = &fakeCheckpointer{
		manifest: testRunCheckpointWaitManifest(), workspaceCapture: testCheckpointWorkspaceCapture(),
	}

	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, readyErr) || !errors.Is(err, failureErr) {
		t.Fatalf("err = %v, want ready and failure admission errors", err)
	}
}

func TestControlPlaneRunWaitsStaysDetachedWhenCheckpointSourceReleaseFails(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 3, CheckpointID: "checkpoint-1",
		}},
	}
	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.Checkpointer = &fakeCheckpointer{
		manifest: testRunCheckpointWaitManifest(), workspaceCapture: testCheckpointWorkspaceCapture(),
		releaseErr: errors.New("close failed"),
	}

	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) || !strings.Contains(err.Error(), "release checkpoint source: close failed") {
		t.Fatalf("err = %v, want detached release failure", err)
	}
}

func TestControlPlaneRunWaitsContinuesAlreadyOpenedWaitWithoutCreatingAnother(t *testing.T) {
	client := &fakeRunWaitClient{
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			ResumeKind: "completed", ResumePayload: json.RawMessage(`{"ok":true}`),
		}},
	}
	var resumed WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindChild)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		resumed = decision
		return nil
	}
	err := (ControlPlaneRunWaits{Client: client}).ContinueRunWait(
		context.Background(),
		request,
		liveRunWaitResponse(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if client.createdRequest.CorrelationID != "" {
		t.Fatalf("unexpected duplicate create request = %+v", client.createdRequest)
	}
	if resumed.Kind != "completed" || string(resumed.Data) != `{"ok":true}` {
		t.Fatalf("resume = %+v", resumed)
	}
}

func TestControlPlaneRunWaitsCapturesWorkspaceForTypedCheckpointIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 2, CheckpointID: "checkpoint-1", CaptureWorkspace: true,
		}},
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRunCheckpointWaitManifest(),
		workspaceCapture: testCheckpointWorkspaceCapture(),
	}

	request := testWaitRequest(workerapi.RunWaitKindTimer)
	request.Checkpointer = checkpointer
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.WorkspaceCapture.Artifact.Digest != "sha256:workspace-capture" ||
		client.ready.WorkspaceCapture.Tree.Digest != "sha256:workspace-tree" {
		t.Fatalf("ready request = %+v", client.ready)
	}
}

func TestControlPlaneRunWaitsResumesAndAcknowledgesTypedVersion(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			RequestVersion: 7, ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`), RequireAck: true,
		}},
	}
	var got WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindActorInput)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		got = decision
		return nil
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "completed" || string(got.Data) != `{"approved":true}` {
		t.Fatalf("resume decision = %+v", got)
	}
	if client.resumeAck == nil || client.resumeAck.ResumeRequestVersion != 7 {
		t.Fatalf("resume acknowledgement = %+v", client.resumeAck)
	}
	if len(client.pollRequests) != 1 || client.pollRequests[0].RunWaitID != "run-wait-id-1" {
		t.Fatalf("poll requests = %+v", client.pollRequests)
	}
}

func TestControlPlaneRunWaitsReturnsImmediateResumeDecision(t *testing.T) {
	immediate := liveRunWaitResponse()
	immediate.ResolutionKind = "completed"
	immediate.Resolution = json.RawMessage(`{"approved":true}`)
	client := &fakeRunWaitClient{created: immediate}
	var got WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindActorInput)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		got = decision
		return nil
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "completed" || string(got.Data) != `{"approved":true}` {
		t.Fatalf("resume decision = %+v", got)
	}
	if len(client.pollRequests) != 0 || client.resumeAck != nil {
		t.Fatalf("immediate resume unexpectedly polled or acknowledged: polls=%d ack=%+v", len(client.pollRequests), client.resumeAck)
	}
}

func TestControlPlaneRunWaitsRejectsMismatchedTypedIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "another-run", RunWaitID: "run-wait-id-1", Status: "waiting",
		}},
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(
		context.Background(), testWaitRequest(workerapi.RunWaitKindTimer),
	)
	if err == nil || !strings.Contains(err.Error(), "mismatched fence") {
		t.Fatalf("err = %v, want mismatched fence", err)
	}
}

func TestControlPlaneRunWaitsRejectsMismatchedCreationIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*workerapi.CreateRunWaitResponse)
	}{
		{name: "run", change: func(response *workerapi.CreateRunWaitResponse) {
			response.RunID = "another-run"
		}},
		{name: "wait", change: func(response *workerapi.CreateRunWaitResponse) {
			response.RunWaitID = "another-wait"
		}},
		{name: "resume attach", change: func(response *workerapi.CreateRunWaitResponse) {
			response.ResumeAttachID = "another-attach"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := liveRunWaitResponse()
			response.ResolutionKind = "completed"
			test.change(&response)
			err := ControlPlaneRunWaits{Client: &fakeRunWaitClient{created: response}}.Wait(
				context.Background(), testWaitRequest(workerapi.RunWaitKindTimer),
			)
			if err == nil || !strings.Contains(err.Error(), "exact request identity") {
				t.Fatalf("error = %v, want exact identity rejection", err)
			}
		})
	}
}

func TestControlPlaneRunWaitsRecordsTypedCheckpointFailure(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 5, CheckpointID: "checkpoint-1",
		}},
	}
	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.Checkpointer = &fakeCheckpointer{err: errors.New("snapshot failed")}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached after attempt-fatal checkpoint failure", err)
	}
	if client.failed == nil || client.failed.RequestVersion != 5 || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
	}
}

func TestControlPlaneRunWaitsRetriesExactCheckpointFailureRequest(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 6, CheckpointID: "checkpoint-1",
		}},
		checkpointFailureErrors: []error{errors.New("temporary checkpoint failure transport error"), nil},
	}
	request := testWaitRequest(workerapi.RunWaitKindToken)
	request.Checkpointer = &fakeCheckpointer{err: errors.New("snapshot failed")}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if len(client.checkpointFailureRequests) != 2 ||
		client.checkpointFailureRequests[0] != client.checkpointFailureRequests[1] {
		t.Fatalf("checkpoint failure retries = %+v, want two exact requests", client.checkpointFailureRequests)
	}
}

func TestControlPlaneRunWaitsUsesCurrentLeaseForCheckpointCompletion(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 1, CheckpointID: "checkpoint-1",
		}},
	}
	leases := &mutableRunLeaseProvider{assignment: testWaitRunLeaseAssignment()}
	checkpointer := &fakeCheckpointer{manifest: testRunCheckpointWaitManifest(), workspaceCapture: testCheckpointWorkspaceCapture(), onCreate: func() {
		leases.assignment.ID = "lease-2"
	}}
	request := testWaitRequest(workerapi.RunWaitKindTimer)
	request.LeaseAssignment = workerapi.RunLeaseAssignment{}
	request.Leases = leases
	request.Checkpointer = checkpointer
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.createdRequest.Lease.ID != "lease-1" || client.ready == nil || client.ready.Lease.ID != "lease-2" {
		t.Fatalf("created lease=%q ready=%+v", client.createdRequest.Lease.ID, client.ready)
	}
}

func TestControlPlaneRunWaitsReleasesOnlyExactGuestResumeProof(t *testing.T) {
	client := &fakeRunWaitClient{}
	assignment := workerapi.RunLeaseAssignment{ID: "lease-2", RunID: "run-1", AttemptNumber: 2}
	err := (ControlPlaneRunWaits{Client: client}).AcknowledgeRestore(context.Background(), RestoreAcknowledgement{
		Lease: assignment, RunWaitID: "wait-1", CheckpointID: "checkpoint-1",
		ResumeAttachID: "attach-1", ResumeRequestVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeRelease == nil || client.resumeRelease.Lease != assignment.Fence() ||
		client.resumeRelease.ResumeAttachID != "attach-1" || client.resumeRelease.ResumeRequestVersion != 4 {
		t.Fatalf("resume release = %+v", client.resumeRelease)
	}
}

type fakeRunWaitClient struct {
	created                   workerapi.CreateRunWaitResponse
	polls                     []workerapi.RunWaitPollResponse
	createdRequest            workerapi.CreateRunWaitRequest
	pollRequests              []workerapi.RunWaitPollRequest
	resumeAck                 *workerapi.RunWaitResumeAckRequest
	ready                     *workerapi.CheckpointReadyRequest
	failed                    *workerapi.CheckpointFailedRequest
	checkpointFailureRequests []workerapi.CheckpointFailedRequest
	checkpointFailureErrors   []error
	resumeRelease             *workerapi.RunResumeReleaseRequest
	readyErrors               []error
	onReady                   func()
}

func (c *fakeRunWaitClient) CreateRunWait(_ context.Context, request workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	c.createdRequest = request
	return c.created, nil
}

func (c *fakeRunWaitClient) PollRunWait(_ context.Context, request workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	c.pollRequests = append(c.pollRequests, request)
	if len(c.polls) == 0 {
		return workerapi.RunWaitPollResponse{}, errors.New("unexpected run wait poll")
	}
	response := c.polls[0]
	c.polls = c.polls[1:]
	return response, nil
}

func (c *fakeRunWaitClient) AcknowledgeRunWaitResume(_ context.Context, request workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	c.resumeAck = &request
	return workerapi.RunWaitResumeAckResponse{
		RunID: "run-1", RunWaitID: request.RunWaitID,
		ResumeRequestVersion: request.ResumeRequestVersion,
	}, nil
}

func (c *fakeRunWaitClient) AcknowledgeRunResumeRelease(_ context.Context, request workerapi.RunResumeReleaseRequest) (workerapi.RunResumeReleaseResponse, error) {
	c.resumeRelease = &request
	return workerapi.RunResumeReleaseResponse(request), nil
}

func (c *fakeRunWaitClient) MarkCheckpointReady(_ context.Context, request workerapi.CheckpointReadyRequest) (workerapi.CheckpointResponse, error) {
	c.ready = &request
	if c.onReady != nil {
		c.onReady()
	}
	if len(c.readyErrors) > 0 {
		err := c.readyErrors[0]
		c.readyErrors = c.readyErrors[1:]
		if err != nil {
			return workerapi.CheckpointResponse{}, err
		}
	}
	return workerapi.CheckpointResponse{
		RunID:     request.Manifest.RecoveryPoint.RunID,
		RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointFailed(_ context.Context, request workerapi.CheckpointFailedRequest) (workerapi.CheckpointResponse, error) {
	c.failed = &request
	c.checkpointFailureRequests = append(c.checkpointFailureRequests, request)
	if len(c.checkpointFailureErrors) > 0 {
		err := c.checkpointFailureErrors[0]
		c.checkpointFailureErrors = c.checkpointFailureErrors[1:]
		if err != nil {
			return workerapi.CheckpointResponse{}, err
		}
	}
	return workerapi.CheckpointResponse{
		RunID: "run-1", RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
	}, nil
}

type fakeCheckpointer struct {
	manifest          workerapi.CheckpointManifest
	workspaceCapture  *CheckpointWorkspaceCapture
	request           CheckpointRequest
	err               error
	onCreate          func()
	releaseCount      int
	releaseErr        error
	releaseContextErr error
}

func (c *fakeCheckpointer) CreateCheckpoint(_ context.Context, request CheckpointRequest) (CheckpointResult, error) {
	c.request = request
	if c.onCreate != nil {
		c.onCreate()
	}
	if c.err != nil {
		return CheckpointResult{}, c.err
	}
	return CheckpointResult{Manifest: c.manifest, WorkspaceCapture: c.workspaceCapture}, nil
}

func (c *fakeCheckpointer) ReleaseCheckpointSource(ctx context.Context) error {
	c.releaseCount++
	c.releaseContextErr = ctx.Err()
	return c.releaseErr
}

type mutableRunLeaseProvider struct {
	assignment workerapi.RunLeaseAssignment
}

func (p *mutableRunLeaseProvider) CurrentWorkerRunLease() workerapi.RunLease {
	return workerRunLeaseFromAssignment("", p.assignment)
}
func (p *mutableRunLeaseProvider) CurrentWorkerRunLeaseAssignment() workerapi.RunLeaseAssignment {
	return p.assignment
}

func liveRunWaitResponse() workerapi.CreateRunWaitResponse {
	return workerapi.CreateRunWaitResponse{
		RunID: "run-1", RunWaitID: "run-wait-id-1", ResumeAttachID: "resume-attach-1",
		RuntimeInstanceID: "runtime-instance-1", RuntimeEpoch: 42,
	}
}

func testWaitRequest(kind workerapi.RunWaitKind) WaitRequest {
	return WaitRequest{
		LeaseAssignment: testWaitRunLeaseAssignment(),
		CorrelationID:   "correlation-1",
		RunWaitID:       "run-wait-id-1",
		ResumeAttachID:  "resume-attach-1",
		Kind:            kind,
	}
}

func testWaitRunLeaseAssignment() workerapi.RunLeaseAssignment {
	return workerapi.RunLeaseAssignment{
		ID: "lease-1", RunID: "run-1", AttemptNumber: 2, WorkerGroupID: "run-us-east-1",
		WorkerInstanceID: "worker-1", WorkerEpoch: 42, LeaseSequence: 1,
		RuntimeInstanceID: "runtime-instance-1",
	}
}

func testCheckpointWorkspaceCapture() *CheckpointWorkspaceCapture {
	return &CheckpointWorkspaceCapture{
		Tree: workspace.TreeIdentity{Digest: "sha256:workspace-tree", SizeBytes: 21, EntryCount: 2},
		Artifact: workspace.WorkspaceArtifact{
			Digest: "sha256:workspace-capture", MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 42, EntryCount: 2,
		},
	}
}

func testRunCheckpointWaitManifest() workerapi.CheckpointManifest {
	return workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{Runtime: workerapi.CheckpointRuntime{
			Backend: "firecracker", ID: "sha256:runtime", Arch: "x86_64", Contract: "helmr.vm-runtime.v0",
			KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", ConfigDigest: "sha256:runtime-config",
			VMVCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("8", 64),
		}},
		RuntimeState: workerapi.CheckpointRuntimeState{
			ConfigArtifact:      workerapi.CheckpointArtifact{Digest: "sha256:" + strings.Repeat("4", 64), MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     workerapi.CheckpointArtifact{Digest: "sha256:" + strings.Repeat("1", 64), MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: workerapi.CheckpointArtifact{Digest: "sha256:" + strings.Repeat("3", 64), MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []workerapi.CheckpointArtifact{{Digest: "sha256:" + strings.Repeat("2", 64), MediaType: cas.CheckpointMemoryMediaType}},
			Config:              json.RawMessage(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		},
	}
}
