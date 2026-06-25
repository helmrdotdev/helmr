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

func TestControlRunWaitsDetachesAfterCheckpointReady(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:              "run-1",
			RunWaitID:          "run-wait-id-1",
			CheckpointID:       "checkpoint-1",
			WorkspaceVersionID: "workspace-version-1",
		},
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRunWaitCheckpointManifest(),
		workspaceCapture: testRunWaitWorkspaceCapture(),
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:          api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID:  "approval-1",
		Kind:           api.WorkerRunWaitKindToken,
		Params:         json.RawMessage(`{"message":"ship it"}`),
		ActiveDuration: 1500 * time.Millisecond,
		Checkpointer:   checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.Manifest.RecoveryPoint.Runtime.Backend != "firecracker" || client.ready.Manifest.RuntimeState.VMStateArtifact.Digest == "" {
		t.Fatalf("ready request = %+v", client.ready)
	}
	if client.ready.RunWaitID != "run-wait-id-1" {
		t.Fatalf("ready ids = %+v", client.ready)
	}
	if client.ready.ActiveDurationMs != 1500 {
		t.Fatalf("active duration ms = %d", client.ready.ActiveDurationMs)
	}
	if checkpointer.request.CheckpointID != "checkpoint-1" {
		t.Fatalf("checkpointer = %+v", checkpointer)
	}
	if checkpointer.request.CaptureWorkspace {
		t.Fatalf("capture workspace flag = true, want false for clean workspace version")
	}
	if client.capture != nil {
		t.Fatalf("unexpected workspace capture = %+v", client.capture)
	}
	if client.failed != nil {
		t.Fatalf("unexpected failed checkpoint = %+v", client.failed)
	}
}

func TestControlRunWaitsUsesCurrentLeaseAfterCheckpoint(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:        "run-1",
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		},
	}
	leases := &mutableRunLeaseProvider{lease: api.WorkerRunLease{
		ID:               "lease-1",
		RunID:            "run-1",
		WorkerInstanceID: "worker-1",
	}}
	checkpointer := &fakeCheckpointer{
		manifest: testRunWaitCheckpointManifest(),
		onCreate: func() {
			leases.lease.ID = "lease-2"
		},
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Leases:        leases,
		CorrelationID: "timer-1",
		Kind:          api.WorkerRunWaitKindTimer,
		Checkpointer:  checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.createdRequest.Lease.ID != "lease-1" {
		t.Fatalf("create wait lease = %s, want lease-1", client.createdRequest.Lease.ID)
	}
	if client.ready == nil || client.ready.Lease.ID != "lease-2" {
		t.Fatalf("ready lease = %+v, want lease-2", client.ready)
	}
}

func TestControlRunWaitsCapturesDirtyWorkspaceBeforeCheckpointReady(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:            "run-1",
			RunWaitID:        "run-wait-id-1",
			CheckpointID:     "checkpoint-1",
			CaptureWorkspace: true,
		},
	}
	checkpointer := &fakeCheckpointer{
		manifest: testRunWaitCheckpointManifest(),
		workspaceCapture: &workspace.WorkspaceArtifact{
			Digest:     "sha256:workspace-capture",
			MediaType:  workspace.ArtifactMediaType,
			Encoding:   workspace.ArtifactEncoding,
			SizeBytes:  42,
			EntryCount: 2,
		},
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		Kind:         api.WorkerRunWaitKindTimer,
		Checkpointer: checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if !checkpointer.request.CaptureWorkspace {
		t.Fatalf("capture workspace flag = false, want true")
	}
	if client.capture == nil || client.capture.WorkspaceCapture.Digest != "sha256:workspace-capture" {
		t.Fatalf("capture request = %+v", client.capture)
	}
	if client.ready == nil {
		t.Fatal("checkpoint ready was not called after capture")
	}
	if client.failed != nil {
		t.Fatalf("unexpected failed checkpoint = %+v", client.failed)
	}
}

func TestControlRunWaitsDoesNotResumeAfterCheckpointReadyError(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:        "run-1",
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		},
		readyErr: errors.New("connection reset"),
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRunWaitCheckpointManifest(),
		workspaceCapture: testRunWaitWorkspaceCapture(),
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindToken,
		Params:        json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if err == nil || !strings.Contains(err.Error(), "mark checkpoint ready") {
		t.Fatalf("err = %v", err)
	}
	if client.ready == nil {
		t.Fatal("checkpoint ready was not attempted")
	}
	if client.failed == nil || client.failed.RunWaitID != "run-wait-id-1" || client.failed.CheckpointID != "checkpoint-1" || !strings.Contains(client.failed.Error, "connection reset") {
		t.Fatalf("failed request = %+v", client.failed)
	}
}

func TestControlRunWaitsReturnsImmediateResumeDecision(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:          "run-1",
			RunWaitID:      "run-wait-id-1",
			CheckpointID:   "checkpoint-1",
			ResolutionKind: "completed",
			Resolution:     json.RawMessage(`{"approved":true}`),
		},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindStream,
		Params:        json.RawMessage(`{"stream":"approval"}`),
		Checkpointer:  &fakeCheckpointer{manifest: testRunWaitCheckpointManifest()},
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
	if client.ready != nil || client.failed != nil {
		t.Fatalf("checkpoint calls should not run for immediate resume: ready=%+v failed=%+v", client.ready, client.failed)
	}
}

func TestControlRunWaitsInvalidatesCheckpointWhenSnapshotFails(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:        "run-1",
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		},
	}
	checkpointer := &fakeCheckpointer{err: errors.New("snapshot failed")}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindToken,
		Params:        json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if err == nil {
		t.Fatal("expected snapshot error")
	}
	if client.failed == nil || client.failed.RunWaitID != "run-wait-id-1" || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
	}
	if client.ready != nil {
		t.Fatalf("unexpected ready request = %+v", client.ready)
	}
}

type fakeRunWaitClient struct {
	created        api.WorkerCreateRunWaitResponse
	createdRequest api.WorkerCreateRunWaitRequest
	capture        *api.WorkerRunWaitWorkspaceCaptureRequest
	ready          *api.WorkerCheckpointReadyRequest
	failed         *api.WorkerCheckpointFailedRequest

	captureErr error
	readyErr   error
}

func (c *fakeRunWaitClient) CreateRunWait(_ context.Context, request api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	c.createdRequest = request
	return c.created, nil
}

func (c *fakeRunWaitClient) CaptureRunWaitWorkspace(_ context.Context, request api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error) {
	c.capture = &request
	if c.captureErr != nil {
		return api.WorkerRunWaitWorkspaceCaptureResponse{}, c.captureErr
	}
	return api.WorkerRunWaitWorkspaceCaptureResponse{
		RunID:              request.Lease.RunID,
		RunWaitID:          request.RunWaitID,
		CheckpointID:       request.CheckpointID,
		WorkspaceVersionID: "workspace-version-1",
	}, nil
}

func (c *fakeRunWaitClient) AcknowledgeRestore(_ context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	return api.WorkerAcknowledgeRestoreResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointReady(_ context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCreateRunWaitResponse, error) {
	c.ready = &request
	if c.readyErr != nil {
		return api.WorkerCreateRunWaitResponse{}, c.readyErr
	}
	return api.WorkerCreateRunWaitResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointFailed(_ context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCreateRunWaitResponse, error) {
	c.failed = &request
	return api.WorkerCreateRunWaitResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
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

type mutableRunLeaseProvider struct {
	lease api.WorkerRunLease
}

func (p *mutableRunLeaseProvider) CurrentWorkerRunLease() api.WorkerRunLease {
	return p.lease
}

func testRunWaitCheckpointManifest() api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         "firecracker",
				ID:              "sha256:runtime",
				Arch:            "amd64",
				ABI:             "helmr.firecracker.snapshot.v0",
				KernelDigest:    "sha256:kernel",
				InitramfsDigest: "sha256:initramfs",
				RootfsDigest:    "sha256:rootfs",
				ConfigDigest:    "sha256:runtime-config",
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("4", 64), MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("1", 64), MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("3", 64), MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{{Digest: "sha256:" + strings.Repeat("2", 64), MediaType: cas.CheckpointMemoryMediaType}},
			Config:              json.RawMessage(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		},
	}
}

func testRunWaitWorkspaceCapture() *workspace.WorkspaceArtifact {
	return &workspace.WorkspaceArtifact{
		Digest:     "sha256:workspace-capture",
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  42,
		EntryCount: 2,
	}
}
