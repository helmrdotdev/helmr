package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestControlRunWaitsDetachesAfterTypedCheckpointIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 3, CheckpointID: "checkpoint-1",
		}},
	}
	checkpointer := &fakeCheckpointer{manifest: testRunCheckpointWaitManifest()}
	checkpointer.workspaceCapture = testCheckpointWorkspaceCapture()

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindToken,
		ActiveDuration: 1500 * time.Millisecond, Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.RequestVersion != 3 || client.ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready request = %+v", client.ready)
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

func TestControlRunWaitsCapturesWorkspaceForTypedCheckpointIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 2, CheckpointID: "checkpoint-1", CaptureWorkspace: true,
		}},
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRunCheckpointWaitManifest(),
		workspaceCapture: testCheckpointWorkspaceCapture(),
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindTimer, Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.WorkspaceCapture.Artifact.Digest != "sha256:workspace-capture" ||
		client.ready.WorkspaceCapture.Tree.Digest != "sha256:workspace-tree" {
		t.Fatalf("ready request = %+v", client.ready)
	}
}

func TestControlRunWaitsResumesAndAcknowledgesTypedVersion(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			RequestVersion: 7, ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`), RequireAck: true,
		}},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindStream,
		Resume: func(_ context.Context, decision WaitResumeDecision) error {
			got = decision
			return nil
		},
	})
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

func TestControlRunWaitsReturnsImmediateResumeDecision(t *testing.T) {
	client := &fakeRunWaitClient{created: api.WorkerCreateRunWaitResponse{
		RunID: "run-1", ResolutionKind: "completed", Resolution: json.RawMessage(`{"approved":true}`),
	}}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindStream,
		Resume: func(_ context.Context, decision WaitResumeDecision) error {
			got = decision
			return nil
		},
	})
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

func TestControlRunWaitsRejectsMismatchedTypedIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "another-run", RunWaitID: "run-wait-id-1", Status: "waiting",
		}},
	}
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindTimer,
	})
	if err == nil || !strings.Contains(err.Error(), "mismatched fence") {
		t.Fatalf("err = %v, want mismatched fence", err)
	}
}

func TestControlRunWaitsRecordsTypedCheckpointFailure(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 5, CheckpointID: "checkpoint-1",
		}},
	}
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindToken,
		Checkpointer: &fakeCheckpointer{err: errors.New("snapshot failed")},
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached after attempt-fatal checkpoint failure", err)
	}
	if client.failed == nil || client.failed.RequestVersion != 5 || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
	}
}

func TestControlRunWaitsRetriesExactCheckpointFailureRequest(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 6, CheckpointID: "checkpoint-1",
		}},
		checkpointFailureErrors: []error{errors.New("temporary checkpoint failure transport error"), nil},
	}
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		LeaseReceipt: testWaitRunLeaseReceipt(), Kind: api.WorkerRunWaitKindToken,
		Checkpointer: &fakeCheckpointer{err: errors.New("snapshot failed")},
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if len(client.checkpointFailureRequests) != 2 ||
		client.checkpointFailureRequests[0] != client.checkpointFailureRequests[1] {
		t.Fatalf("checkpoint failure retries = %+v, want two exact requests", client.checkpointFailureRequests)
	}
}

func TestControlRunWaitsUsesCurrentLeaseForCheckpointCompletion(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "checkpoint_requested",
			RequestVersion: 1, CheckpointID: "checkpoint-1",
		}},
	}
	leases := &mutableRunLeaseProvider{receipt: testWaitRunLeaseReceipt()}
	checkpointer := &fakeCheckpointer{manifest: testRunCheckpointWaitManifest(), workspaceCapture: testCheckpointWorkspaceCapture(), onCreate: func() {
		leases.receipt.ID = "lease-2"
	}}
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Leases: leases, Kind: api.WorkerRunWaitKindTimer, Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.createdRequest.Lease.ID != "lease-1" || client.ready == nil || client.ready.Lease.ID != "lease-2" {
		t.Fatalf("created lease=%q ready=%+v", client.createdRequest.Lease.ID, client.ready)
	}
}

func TestControlRunWaitsReleasesOnlyExactGuestResumeProof(t *testing.T) {
	client := &fakeRunWaitClient{}
	receipt := api.WorkerRunLeaseReceipt{ID: "lease-2", RunID: "run-1", AttemptNumber: 2}
	err := (ControlRunWaits{Client: client}).AcknowledgeRestore(context.Background(), RestoreAcknowledgement{
		Lease: receipt, RunWaitID: "wait-1", CheckpointID: "checkpoint-1",
		ResumeAttachID: "attach-1", ResumeRequestVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeRelease == nil || client.resumeRelease.RunLeaseID != "lease-2" ||
		client.resumeRelease.ResumeAttachID != "attach-1" || client.resumeRelease.ResumeRequestVersion != 4 {
		t.Fatalf("resume release = %+v", client.resumeRelease)
	}
}

type fakeRunWaitClient struct {
	created                   api.WorkerCreateRunWaitResponse
	polls                     []api.WorkerRunWaitPollResponse
	createdRequest            api.WorkerCreateRunWaitRequest
	pollRequests              []api.WorkerRunWaitPollRequest
	resumeAck                 *api.WorkerRunWaitResumeAckRequest
	ready                     *api.WorkerCheckpointReadyRequest
	failed                    *api.WorkerCheckpointFailedRequest
	checkpointFailureRequests []api.WorkerCheckpointFailedRequest
	checkpointFailureErrors   []error
	resumeRelease             *api.WorkerRunResumeReleaseRequest
}

func (c *fakeRunWaitClient) CreateRunWait(_ context.Context, request api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	c.createdRequest = request
	return c.created, nil
}

func (c *fakeRunWaitClient) PollRunWait(_ context.Context, request api.WorkerRunWaitPollRequest) (api.WorkerRunWaitPollResponse, error) {
	c.pollRequests = append(c.pollRequests, request)
	if len(c.polls) == 0 {
		return api.WorkerRunWaitPollResponse{}, errors.New("unexpected run wait poll")
	}
	response := c.polls[0]
	c.polls = c.polls[1:]
	return response, nil
}

func (c *fakeRunWaitClient) AcknowledgeRunWaitResume(_ context.Context, request api.WorkerRunWaitResumeAckRequest) (api.WorkerRunWaitResumeAckResponse, error) {
	c.resumeAck = &request
	return api.WorkerRunWaitResumeAckResponse{
		RunID: request.Lease.RunID, RunWaitID: request.RunWaitID,
		ResumeRequestVersion: request.ResumeRequestVersion,
	}, nil
}

func (c *fakeRunWaitClient) AcknowledgeRestore(_ context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	return api.WorkerAcknowledgeRestoreResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

func (c *fakeRunWaitClient) AcknowledgeRunResumeRelease(_ context.Context, request api.WorkerRunResumeReleaseRequest) (api.WorkerRunResumeReleaseResponse, error) {
	c.resumeRelease = &request
	return api.WorkerRunResumeReleaseResponse{
		Lease: request.Lease, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
		ResumeAttachID: request.ResumeAttachID, ResumeRequestVersion: request.ResumeRequestVersion,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointReady(_ context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error) {
	c.ready = &request
	return api.WorkerCheckpointResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointFailed(_ context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error) {
	c.failed = &request
	c.checkpointFailureRequests = append(c.checkpointFailureRequests, request)
	if len(c.checkpointFailureErrors) > 0 {
		err := c.checkpointFailureErrors[0]
		c.checkpointFailureErrors = c.checkpointFailureErrors[1:]
		if err != nil {
			return api.WorkerCheckpointResponse{}, err
		}
	}
	return api.WorkerCheckpointResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

type fakeCheckpointer struct {
	manifest         api.WorkerCheckpointManifest
	workspaceCapture *CheckpointWorkspaceCapture
	request          CheckpointRequest
	err              error
	onCreate         func()
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

type mutableRunLeaseProvider struct{ receipt api.WorkerRunLeaseReceipt }

func (p *mutableRunLeaseProvider) CurrentWorkerRunLease() api.WorkerRunLease {
	return workerRunLeaseFromReceipt("", p.receipt)
}
func (p *mutableRunLeaseProvider) CurrentWorkerRunLeaseReceipt() api.WorkerRunLeaseReceipt {
	return p.receipt
}

func liveRunWaitResponse() api.WorkerCreateRunWaitResponse {
	return api.WorkerCreateRunWaitResponse{
		RunID: "run-1", RunWaitID: "run-wait-id-1", ResumeAttachID: "resume-attach-1",
		RuntimeInstanceID: "runtime-instance-1", RuntimeEpoch: 42,
	}
}

func testWaitRunLeaseReceipt() api.WorkerRunLeaseReceipt {
	return api.WorkerRunLeaseReceipt{
		ID: "lease-1", RunID: "run-1", AttemptNumber: 2, WorkerGroupID: "run-us-east-1",
		WorkerInstanceID: "worker-1", WorkerEpoch: 42, LeaseSequence: 1,
		RuntimeInstanceID: "runtime-instance-1", NetworkSlotID: "network-slot-1", NetworkSlotGeneration: 1,
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

func testRunCheckpointWaitManifest() api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{Runtime: api.WorkerCheckpointRuntime{
			Backend: "firecracker", ID: "sha256:runtime", Arch: "x86_64", ABI: "helmr.firecracker.snapshot.v0",
			KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", ConfigDigest: "sha256:runtime-config",
		}},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("4", 64), MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("1", 64), MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("3", 64), MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{{Digest: "sha256:" + strings.Repeat("2", 64), MediaType: cas.CheckpointMemoryMediaType}},
			Config:              json.RawMessage(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		},
	}
}
