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
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{checkpointDueCommand(1, "run-wait-id-1")},
		claim: api.WorkerCheckpointClaimResponse{
			RunID:              "run-1",
			RunWaitID:          "run-wait-id-1",
			CheckpointID:       "checkpoint-1",
			WorkspaceVersionID: "workspace-version-1",
		},
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRuntimeCheckpointWaitManifest(),
		workspaceCapture: testRunWaitWorkspaceCapture(),
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:          testRunLease(),
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
	if client.ready.WorkerCommandID != 1 {
		t.Fatalf("ready worker command id = %d, want 1", client.ready.WorkerCommandID)
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
	if len(client.acked) != 1 || client.acked[0] != 1 {
		t.Fatalf("acked = %+v", client.acked)
	}
	if len(client.accepted) != 1 || client.accepted[0] != 1 {
		t.Fatalf("accepted = %+v", client.accepted)
	}
}

func TestControlRunWaitsUsesCurrentLeaseAfterCheckpoint(t *testing.T) {
	client := &fakeRunWaitClient{
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{checkpointDueCommand(1, "run-wait-id-1")},
		claim: api.WorkerCheckpointClaimResponse{
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
		manifest: testRuntimeCheckpointWaitManifest(),
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
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{checkpointDueCommand(1, "run-wait-id-1")},
		claim: api.WorkerCheckpointClaimResponse{
			RunID:            "run-1",
			RunWaitID:        "run-wait-id-1",
			CheckpointID:     "checkpoint-1",
			CaptureWorkspace: true,
		},
	}
	checkpointer := &fakeCheckpointer{
		manifest: testRuntimeCheckpointWaitManifest(),
		workspaceCapture: &workspace.WorkspaceArtifact{
			Digest:     "sha256:workspace-capture",
			MediaType:  workspace.ArtifactMediaType,
			Encoding:   workspace.ArtifactEncoding,
			SizeBytes:  42,
			EntryCount: 2,
		},
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        testRunLease(),
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

func TestControlRunWaitsContinuesAfterCheckpointReadyError(t *testing.T) {
	client := &fakeRunWaitClient{
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{checkpointDueCommand(1, "run-wait-id-1")},
		claim: api.WorkerCheckpointClaimResponse{
			RunID:        "run-1",
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		},
		readyErr: errors.New("connection reset"),
	}
	checkpointer := &fakeCheckpointer{
		manifest:         testRuntimeCheckpointWaitManifest(),
		workspaceCapture: testRunWaitWorkspaceCapture(),
	}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         testRunLease(),
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindToken,
		Params:        json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled after checkpoint attempt failure was absorbed", err)
	}
	if client.ready == nil {
		t.Fatal("checkpoint ready was not attempted")
	}
	if client.failed == nil || client.failed.RunWaitID != "run-wait-id-1" || client.failed.CheckpointID != "checkpoint-1" || !strings.Contains(client.failed.Error, "connection reset") {
		t.Fatalf("failed request = %+v", client.failed)
	}
	if len(client.acked) != 1 || client.acked[0] != 1 {
		t.Fatalf("acked = %+v, want checkpoint command acked after failed attempt was recorded", client.acked)
	}
}

func TestControlRunWaitsReturnsImmediateResumeDecision(t *testing.T) {
	client := &fakeRunWaitClient{
		created: api.WorkerCreateRunWaitResponse{
			RunID:          "run-1",
			RunWaitID:      "run-wait-id-1",
			ResolutionKind: "completed",
			Resolution:     json.RawMessage(`{"approved":true}`),
		},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         testRunLease(),
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindStream,
		Params:        json.RawMessage(`{"stream":"approval"}`),
		Checkpointer:  &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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

func TestControlRunWaitsResumesFromWorkerCommandStream(t *testing.T) {
	client := &fakeRunWaitClient{
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{resumeDecisionCommand(7, "run-wait-id-1", "completed", `{"approved":true}`)},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         testRunLease(),
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindStream,
		Params:        json.RawMessage(`{"stream":"approval"}`),
		Checkpointer:  &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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
	if len(client.acked) != 1 || client.acked[0] != 7 {
		t.Fatalf("acked = %+v", client.acked)
	}
	if len(client.accepted) != 1 || client.accepted[0] != 7 {
		t.Fatalf("accepted = %+v", client.accepted)
	}
	if client.ready != nil || client.failed != nil {
		t.Fatalf("checkpoint calls should not run for hot resume: ready=%+v failed=%+v", client.ready, client.failed)
	}
}

func TestControlRunWaitsSkipsStaleEpochResumeCommand(t *testing.T) {
	stale := resumeDecisionCommand(7, "run-wait-id-1", "completed", `{"approved":false}`)
	stale.RuntimeEpoch = 41
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		commandBatches: [][]api.WorkerCommand{
			{stale},
			{resumeDecisionCommand(8, "run-wait-id-1", "completed", `{"approved":true}`)},
		},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        testRunLease(),
		Kind:         api.WorkerRunWaitKindStream,
		Checkpointer: &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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
	if len(client.acked) != 2 || client.acked[0] != 7 || client.acked[1] != 8 {
		t.Fatalf("acked = %+v", client.acked)
	}
	if len(client.accepted) != 1 || client.accepted[0] != 8 {
		t.Fatalf("accepted = %+v, want only the non-stale command", client.accepted)
	}
}

func TestControlRunWaitsDoesNotRetryResumeWhenAckFailsAfterResume(t *testing.T) {
	client := &fakeRunWaitClient{
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{resumeDecisionCommand(7, "run-wait-id-1", "completed", `{"approved":true}`)},
		ackErr:   errors.New("ack unavailable"),
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        testRunLease(),
		Kind:         api.WorkerRunWaitKindStream,
		Checkpointer: &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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
	if len(client.acked) != 1 || client.acked[0] != 7 {
		t.Fatalf("acked = %+v", client.acked)
	}
}

func TestControlRunWaitsReconnectsWorkerCommandStreamAfterCleanEOF(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		commandBatches: [][]api.WorkerCommand{
			{resumeDecisionCommand(3, "other-wait-id", "completed", `null`)},
			{resumeDecisionCommand(9, "run-wait-id-1", "completed", `{"approved":true}`)},
		},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        testRunLease(),
		Kind:         api.WorkerRunWaitKindStream,
		Checkpointer: &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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
	if len(client.followAfterIDs) != 2 || client.followAfterIDs[0] != 0 || client.followAfterIDs[1] != 3 {
		t.Fatalf("follow after ids = %+v", client.followAfterIDs)
	}
	if len(client.acked) != 1 || client.acked[0] != 9 {
		t.Fatalf("acked = %+v", client.acked)
	}
}

func TestControlRunWaitsSkipsStaleCheckpointDueAndWaitsForResume(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		claim: api.WorkerCheckpointClaimResponse{
			RunID:     "run-1",
			RunWaitID: "run-wait-id-1",
			Status:    "stale",
		},
		commands: []api.WorkerCommand{
			checkpointDueCommand(1, "run-wait-id-1"),
			resumeDecisionCommand(2, "run-wait-id-1", "completed", `{"approved":true}`),
		},
	}
	var got WaitResumeDecision
	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:        testRunLease(),
		Kind:         api.WorkerRunWaitKindToken,
		Checkpointer: &fakeCheckpointer{manifest: testRuntimeCheckpointWaitManifest()},
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
	if len(client.acked) != 2 || client.acked[0] != 1 || client.acked[1] != 2 {
		t.Fatalf("acked = %+v", client.acked)
	}
	if len(client.accepted) != 2 || client.accepted[0] != 1 || client.accepted[1] != 2 {
		t.Fatalf("accepted = %+v", client.accepted)
	}
	if client.ready != nil || client.failed != nil {
		t.Fatalf("checkpoint calls should not run for stale checkpoint due: ready=%+v failed=%+v", client.ready, client.failed)
	}
}

func TestControlRunWaitsInvalidatesCheckpointWhenSnapshotFails(t *testing.T) {
	client := &fakeRunWaitClient{
		created:  liveRunWaitResponse(),
		commands: []api.WorkerCommand{checkpointDueCommand(1, "run-wait-id-1")},
		claim: api.WorkerCheckpointClaimResponse{
			RunID:        "run-1",
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		},
	}
	checkpointer := &fakeCheckpointer{err: errors.New("snapshot failed")}

	err := ControlRunWaits{Client: client}.Wait(context.Background(), WaitRequest{
		Lease:         testRunLease(),
		CorrelationID: "approval-1",
		Kind:          api.WorkerRunWaitKindToken,
		Params:        json.RawMessage(`{"message":"ship it"}`),
		Checkpointer:  checkpointer,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled after checkpoint attempt failure was absorbed", err)
	}
	if client.failed == nil || client.failed.RunWaitID != "run-wait-id-1" || client.failed.CheckpointID != "checkpoint-1" {
		t.Fatalf("failed request = %+v", client.failed)
	}
	if client.failed.WorkerCommandID != 1 {
		t.Fatalf("failed worker command id = %d, want 1", client.failed.WorkerCommandID)
	}
	if client.ready != nil {
		t.Fatalf("unexpected ready request = %+v", client.ready)
	}
	if len(client.acked) != 1 || client.acked[0] != 1 {
		t.Fatalf("acked = %+v, want checkpoint command acked after failed attempt was recorded", client.acked)
	}
}

type fakeRunWaitClient struct {
	created        api.WorkerCreateRunWaitResponse
	claim          api.WorkerCheckpointClaimResponse
	commands       []api.WorkerCommand
	commandBatches [][]api.WorkerCommand
	accepted       []int64
	acked          []int64
	followAfterIDs []int64
	createdRequest api.WorkerCreateRunWaitRequest
	claimRequest   *api.WorkerCheckpointClaimRequest
	capture        *api.WorkerRunWaitWorkspaceCaptureRequest
	ready          *api.WorkerCheckpointReadyRequest
	failed         *api.WorkerCheckpointFailedRequest

	captureErr error
	readyErr   error
	ackErr     error
}

func (c *fakeRunWaitClient) CreateRunWait(_ context.Context, request api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	c.createdRequest = request
	return c.created, nil
}

func (c *fakeRunWaitClient) FollowWorkerCommands(_ context.Context, afterID int64, handle func(api.WorkerCommand) error) error {
	c.followAfterIDs = append(c.followAfterIDs, afterID)
	commands := c.commands
	if len(c.commandBatches) > 0 {
		commands = c.commandBatches[0]
		c.commandBatches = c.commandBatches[1:]
	}
	for _, command := range commands {
		if err := handle(command); err != nil {
			return err
		}
	}
	if len(c.commandBatches) > 0 {
		return nil
	}
	return context.Canceled
}

func (c *fakeRunWaitClient) AcknowledgeWorkerCommand(_ context.Context, id int64) (api.WorkerCommandAckResponse, error) {
	c.acked = append(c.acked, id)
	if c.ackErr != nil {
		return api.WorkerCommandAckResponse{}, c.ackErr
	}
	return api.WorkerCommandAckResponse{ID: id}, nil
}

func (c *fakeRunWaitClient) AcceptWorkerCommand(_ context.Context, id int64) (api.WorkerCommandAcceptResponse, error) {
	c.accepted = append(c.accepted, id)
	return api.WorkerCommandAcceptResponse{ID: id}, nil
}

func (c *fakeRunWaitClient) ClaimRuntimeCheckpointWait(_ context.Context, request api.WorkerCheckpointClaimRequest) (api.WorkerCheckpointClaimResponse, error) {
	c.claimRequest = &request
	if c.claim.RunWaitID == "" {
		c.claim = api.WorkerCheckpointClaimResponse{
			RunID:        request.Lease.RunID,
			RunWaitID:    request.RunWaitID,
			CheckpointID: "checkpoint-1",
		}
	}
	return c.claim, nil
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

func checkpointDueCommand(id int64, runWaitID string) api.WorkerCommand {
	return api.WorkerCommand{
		ID:                id,
		RunID:             "run-1",
		RunWaitID:         runWaitID,
		RunLeaseID:        "lease-1",
		WorkerInstanceID:  "worker-1",
		RuntimeInstanceID: "runtime-instance-1",
		RuntimeEpoch:      42,
		Kind:              "runtime_checkpoint_wait",
	}
}

func resumeDecisionCommand(id int64, runWaitID string, kind string, payload string) api.WorkerCommand {
	body, _ := json.Marshal(map[string]any{
		"resume_kind":    kind,
		"resume_payload": json.RawMessage(payload),
	})
	return api.WorkerCommand{
		ID:                id,
		RunID:             "run-1",
		RunWaitID:         runWaitID,
		RunLeaseID:        "lease-1",
		WorkerInstanceID:  "worker-1",
		RuntimeInstanceID: "runtime-instance-1",
		RuntimeEpoch:      42,
		Kind:              "runtime_resume_wait",
		Payload:           body,
	}
}

func liveRunWaitResponse() api.WorkerCreateRunWaitResponse {
	return api.WorkerCreateRunWaitResponse{
		RunID:             "run-1",
		RunWaitID:         "run-wait-id-1",
		RuntimeInstanceID: "runtime-instance-1",
		RuntimeEpoch:      42,
	}
}

func testRunLease() api.WorkerRunLease {
	return api.WorkerRunLease{
		ID:               "lease-1",
		RunID:            "run-1",
		WorkerInstanceID: "worker-1",
	}
}

func (c *fakeRunWaitClient) AcknowledgeRestore(_ context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	return api.WorkerAcknowledgeRestoreResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointReady(_ context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error) {
	c.ready = &request
	if c.readyErr != nil {
		return api.WorkerCheckpointResponse{}, c.readyErr
	}
	return api.WorkerCheckpointResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func (c *fakeRunWaitClient) MarkCheckpointFailed(_ context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error) {
	c.failed = &request
	return api.WorkerCheckpointResponse{
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

func testRuntimeCheckpointWaitManifest() api.WorkerCheckpointManifest {
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
