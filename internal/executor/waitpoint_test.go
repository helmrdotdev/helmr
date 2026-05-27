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
)

func TestControlWaitpointsDetachesAfterCheckpointReady(t *testing.T) {
	client := &fakeWaitpointClient{
		created: api.WorkerCreateWaitpointResponse{
			RunID:        "run-1",
			WaitpointID:  "waitpoint-1",
			CheckpointID: "checkpoint-1",
		},
	}
	checkpointer := &fakeCheckpointer{
		manifest: testWaitpointCheckpointManifest(),
	}

	err := ControlWaitpoints{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:          api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID:  "approval-1",
		Kind:           api.WorkerWaitpointKindApproval,
		Request:        json.RawMessage(`{"message":"ship it"}`),
		ActiveDuration: 1500 * time.Millisecond,
		Checkpointer:   checkpointer,
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("err = %v, want ErrDetached", err)
	}
	if client.ready == nil || client.ready.Manifest.RecoveryPoint.Runtime.Backend != "firecracker" || client.ready.Manifest.RuntimeState.VMStateArtifact.Digest == "" {
		t.Fatalf("ready request = %+v", client.ready)
	}
	if client.ready.ActiveDurationMs != 1500 {
		t.Fatalf("active duration ms = %d", client.ready.ActiveDurationMs)
	}
	if checkpointer.request.CheckpointID != "checkpoint-1" {
		t.Fatalf("checkpointer = %+v", checkpointer)
	}
	if client.failed != nil {
		t.Fatalf("unexpected failed checkpoint = %+v", client.failed)
	}
}

func TestControlWaitpointsDoesNotResumeAfterCheckpointReadyError(t *testing.T) {
	client := &fakeWaitpointClient{
		created: api.WorkerCreateWaitpointResponse{
			RunID:        "run-1",
			WaitpointID:  "waitpoint-1",
			CheckpointID: "checkpoint-1",
		},
		readyErr: errors.New("connection reset"),
	}
	checkpointer := &fakeCheckpointer{
		manifest: testWaitpointCheckpointManifest(),
	}

	err := ControlWaitpoints{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID: "approval-1",
		Kind:          api.WorkerWaitpointKindApproval,
		Request:       json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if err == nil || !strings.Contains(err.Error(), "mark checkpoint ready") {
		t.Fatalf("err = %v", err)
	}
	if client.ready == nil {
		t.Fatal("checkpoint ready was not attempted")
	}
	if client.failed == nil || client.failed.CheckpointID != "checkpoint-1" || !strings.Contains(client.failed.Error, "connection reset") {
		t.Fatalf("failed request = %+v", client.failed)
	}
}

func TestControlWaitpointsInvalidatesCheckpointWhenSnapshotFails(t *testing.T) {
	client := &fakeWaitpointClient{
		created: api.WorkerCreateWaitpointResponse{
			RunID:        "run-1",
			WaitpointID:  "waitpoint-1",
			CheckpointID: "checkpoint-1",
		},
	}
	checkpointer := &fakeCheckpointer{err: errors.New("snapshot failed")}

	err := ControlWaitpoints{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		CorrelationID: "approval-1",
		Kind:          api.WorkerWaitpointKindApproval,
		Request:       json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if err == nil {
		t.Fatal("expected snapshot error")
	}
	if client.failed == nil || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
	}
	if client.ready != nil {
		t.Fatalf("unexpected ready request = %+v", client.ready)
	}
}

type fakeWaitpointClient struct {
	created  api.WorkerCreateWaitpointResponse
	ready    *api.WorkerCheckpointReadyRequest
	failed   *api.WorkerCheckpointFailedRequest
	readyErr error
}

func (c *fakeWaitpointClient) CreateWaitpoint(context.Context, api.WorkerCreateWaitpointRequest) (api.WorkerCreateWaitpointResponse, error) {
	return c.created, nil
}

func (c *fakeWaitpointClient) AcknowledgeRestore(_ context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	return api.WorkerAcknowledgeRestoreResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  request.WaitpointID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeWaitpointClient) MarkCheckpointReady(_ context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCreateWaitpointResponse, error) {
	c.ready = &request
	if c.readyErr != nil {
		return api.WorkerCreateWaitpointResponse{}, c.readyErr
	}
	return api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  request.WaitpointID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeWaitpointClient) MarkCheckpointFailed(_ context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCreateWaitpointResponse, error) {
	c.failed = &request
	return api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  request.WaitpointID,
		CheckpointID: request.CheckpointID,
	}, nil
}

type fakeCheckpointer struct {
	manifest api.WorkerCheckpointManifest
	request  CheckpointRequest
	err      error
}

func (c *fakeCheckpointer) CreateCheckpoint(_ context.Context, request CheckpointRequest) (api.WorkerCheckpointManifest, error) {
	c.request = request
	if c.err != nil {
		return api.WorkerCheckpointManifest{}, c.err
	}
	return c.manifest, nil
}

func testWaitpointCheckpointManifest() api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			Runtime: api.WorkerCheckpointRuntime{
				Backend: "firecracker",
				Arch:    "amd64",
				ABI:     "helmr.firecracker.snapshot.v0",
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
