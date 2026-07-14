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

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindToken,
		ActiveDuration: 1500 * time.Millisecond, Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.RequestVersion != 3 || client.ready.CheckpointID != "checkpoint-1" {
		t.Fatalf("ready request = %+v", client.ready)
	}
	if client.ready.ActiveDurationMs != 1500 {
		t.Fatalf("active duration ms = %d, want 1500", client.ready.ActiveDurationMs)
	}
	if checkpointer.request.CheckpointID != "checkpoint-1" {
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
		manifest: testRunCheckpointWaitManifest(),
		workspaceCapture: &workspace.WorkspaceArtifact{
			Digest: "sha256:workspace-capture", MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 42, EntryCount: 2,
		},
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindTimer, Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.capture == nil || client.capture.RequestVersion != 2 || client.capture.WorkspaceCapture.Digest != "sha256:workspace-capture" {
		t.Fatalf("capture request = %+v", client.capture)
	}
	if client.ready == nil || client.ready.WorkspaceVersionID != "workspace-version-1" {
		t.Fatalf("ready request = %+v", client.ready)
	}
}

func TestControlRunWaitsResumesAndAcknowledgesTypedVersion(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			RequestVersion: 7, ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`),
		}},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindStream,
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
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindStream,
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
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindTimer,
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
		Lease: testRunLease(), Kind: api.WorkerRunWaitKindToken,
		Checkpointer: &fakeCheckpointer{err: errors.New("snapshot failed")},
	})
	if err != nil {
		t.Fatalf("err = %v, want recorded failure to complete wait handler", err)
	}
	if client.failed == nil || client.failed.RequestVersion != 5 || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
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
	leases := &mutableRunLeaseProvider{lease: testRunLease()}
	checkpointer := &fakeCheckpointer{manifest: testRunCheckpointWaitManifest(), onCreate: func() {
		leases.lease.ID = "lease-2"
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

type fakeRunWaitClient struct {
	created        api.WorkerCreateRunWaitResponse
	polls          []api.WorkerRunWaitPollResponse
	createdRequest api.WorkerCreateRunWaitRequest
	pollRequests   []api.WorkerRunWaitPollRequest
	resumeAck      *api.WorkerRunWaitResumeAckRequest
	capture        *api.WorkerRunWaitWorkspaceCaptureRequest
	ready          *api.WorkerCheckpointReadyRequest
	failed         *api.WorkerCheckpointFailedRequest
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

func (c *fakeRunWaitClient) CaptureRunWaitWorkspace(_ context.Context, request api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error) {
	c.capture = &request
	return api.WorkerRunWaitWorkspaceCaptureResponse{
		RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
		WorkspaceVersionID: "workspace-version-1",
	}, nil
}

func (c *fakeRunWaitClient) AcknowledgeRestore(_ context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	return api.WorkerAcknowledgeRestoreResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointReady(_ context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error) {
	c.ready = &request
	return api.WorkerCheckpointResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointFailed(_ context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error) {
	c.failed = &request
	return api.WorkerCheckpointResponse{RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID}, nil
}

type fakeCheckpointer struct {
	manifest         api.WorkerCheckpointManifest
	workspaceCapture *workspace.WorkspaceArtifact
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

type mutableRunLeaseProvider struct{ lease api.WorkerRunLease }

func (p *mutableRunLeaseProvider) CurrentWorkerRunLease() api.WorkerRunLease { return p.lease }

func liveRunWaitResponse() api.WorkerCreateRunWaitResponse {
	return api.WorkerCreateRunWaitResponse{
		RunID: "run-1", RunWaitID: "run-wait-id-1", RuntimeInstanceID: "runtime-instance-1", RuntimeEpoch: 42,
	}
}

func testRunLease() api.WorkerRunLease {
	return api.WorkerRunLease{
		ID: "lease-1", RunID: "run-1", WorkerGroupID: "run-us-east-1",
		WorkerInstanceID: "worker-1", WorkerEpoch: 42, LeaseSequence: 1,
		RuntimeInstanceID: "runtime-instance-1", NetworkSlotID: "network-slot-1", NetworkSlotGeneration: 1,
	}
}

func testRunCheckpointWaitManifest() api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{Runtime: api.WorkerCheckpointRuntime{
			Backend: "firecracker", ID: "sha256:runtime", Arch: "amd64", ABI: "helmr.firecracker.snapshot.v0",
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
