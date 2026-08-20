package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func TestExecutorCompletesSuccessfulRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := lease
	renewed.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	frozen := renewed
	frozen.ExpiresAt = renewed.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: renewed,
		result: RunLeaseTaskResult{
			Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
				Output: json.RawMessage(`{"ok":true}`),
			}},
			ProgramQuiesced: workerapi.RunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(renewed),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
	}
	runner := &testRunLeaseTaskRunner{trace: trace, task: task}
	executor := Executor{RunLeases: controlPlane, RunLeaseTasks: runner}

	err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{
		LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{
		"claim", "start", "wait", "renew", "begin",
		"guest-begin", "capture", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completed.Workspace.Captured == nil ||
		controlPlane.completed.Workspace.RolledBack != nil ||
		controlPlane.completed.Outcome.Succeeded == nil {
		t.Fatalf("completion = %+v", controlPlane.completed)
	}
}

func TestExecutorRollsBackFailedRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
		result: RunLeaseTaskResult{
			Outcome: workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "failed"}},
			ProgramQuiesced: workerapi.RunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationReset),
	}
	executor := Executor{
		RunLeases:     controlPlane,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}

	err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{
		LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{
		"claim", "start", "wait", "renew", "begin", "guest-begin", "reset", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completed.Workspace.Captured != nil ||
		controlPlane.completed.Workspace.RolledBack == nil ||
		controlPlane.completed.Outcome.Failed == nil {
		t.Fatalf("completion = %+v", controlPlane.completed)
	}
}

func TestExecutorCompletesSuccessfulActorRunLease(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
		result: RunLeaseTaskResult{
			ActorOutcome: &workerapi.ActorOutcome{
				TerminalInputSequence: 4,
				Succeeded:             &workerapi.ActorSucceeded{},
			},
			ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID},
		},
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
	}
	executor := Executor{RunLeases: controlPlane, RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task}}
	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{"claim", "start", "wait", "renew", "begin", "guest-begin", "capture", "complete-actor"}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completedActor.Outcome.Succeeded == nil || controlPlane.completedActor.Outcome.TerminalInputSequence != 4 || controlPlane.completedActor.Workspace.Captured == nil {
		t.Fatalf("Actor completion = %+v", controlPlane.completedActor)
	}
}

func TestExecutorReplaysFinalizationWithStableAuthority(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
		result: RunLeaseTaskResult{
			Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
				Output: json.RawMessage(`null`),
			}},
			ProgramQuiesced: workerapi.RunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
		beginFailures:   1,
		captureFailures: 1,
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:            trace,
		claim:            workerapi.RunLeaseClaimResponse{Lease: lease},
		begin:            testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
		beginFailures:    1,
		completeFailures: 1,
	}
	executor := Executor{
		RunLeases:     controlPlane,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}
	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{
		LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if len(controlPlane.beginOperationIDs) != 2 ||
		controlPlane.beginOperationIDs[0] != controlPlane.beginOperationIDs[1] {
		t.Fatalf("begin operation IDs = %v", controlPlane.beginOperationIDs)
	}
	if !slices.Equal(trace.calls, []string{
		"claim", "start", "wait", "renew", "begin", "begin", "guest-begin",
		"guest-begin", "capture", "capture", "complete", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
}

func TestExecutorRenewalAcceptsCommittedActorWorkspaceFrontier(t *testing.T) {
	trace := &runLeaseTrace{}
	current := testRunLeaseAssignment(time.Now().Add(time.Minute))
	current.BaseWorkspaceVersionID = "version-1"
	previous := current
	previous.BaseWorkspaceVersionID = "version-2"
	previous.ExpiresAt = current.ExpiresAt.Add(30 * time.Second)
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	task := &testRunLeaseTask{trace: trace, previous: previous, renewed: renewed}

	got, err := (Executor{}).renewRunLease(context.Background(), task, current)
	if err != nil {
		t.Fatal(err)
	}
	if !equalRunLeaseAssignment(got, renewed) {
		t.Fatalf("renewed Lease = %+v, want %+v", got, renewed)
	}
}

func TestRenewRunLeaseAuthorityInstallsCommittedRenewalAfterCallerCancellation(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	controlPlane := cancelingRenewalControlPlane{
		cancel:   cancel,
		response: testRunLeaseRenewResponse(renewed),
	}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", RuntimeInstanceID: "runtime-1",
		FencingGeneration: 4, Target: workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: "version-1"},
	}, &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}, "channel-1")
	authority := &workspacev0.WorkspaceRunAuthority{
		Fence: &workspacev0.WorkspaceAuthorityFence{
			WorkspaceMountId: "mount-1", WorkspaceId: "workspace-1",
			RuntimeInstanceId: "runtime-1", MountFencingGeneration: 4,
			RunId: previous.RunID, ExpiresAtUnixNano: previous.ExpiresAt.UnixNano(),
			BaseWorkspaceVersionId: "version-1",
		},
		ChannelToken: "channel-1",
	}
	serverResult := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			serverResult <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceAuthorityRenew {
			serverResult <- errors.New("unexpected renewal stream")
			return
		}
		var request workspacev0.RenewWorkspaceAuthorityRequest
		if err := frameio.ReadProtoFrame(guest, &request); err != nil {
			serverResult <- err
			return
		}
		fence := proto.Clone(request.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
		fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
		serverResult <- frameio.WriteProtoFrame(
			guest,
			&workspacev0.RenewWorkspaceAuthorityResponse{Fence: fence},
		)
	}()
	got, fence, err := renewRunLeaseAuthority(ctx, controlPlane, registry, previous, authority)
	if err != nil {
		t.Fatal(err)
	}
	if !equalRunLeaseAssignment(got, renewed) ||
		fence.GetExpiresAtUnixNano() != renewed.ExpiresAt.UnixNano() {
		t.Fatalf("renewal = (%+v, %+v)", got, fence)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestRenewRunLeaseAuthorityStopsAtGuestAcknowledgedExpiry(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(250 * time.Millisecond))
	controlPlane := &failingRenewalControlPlane{}
	started := time.Now()
	_, _, err := renewRunLeaseAuthority(
		context.Background(),
		controlPlane,
		nil,
		previous,
		&workspacev0.WorkspaceRunAuthority{},
	)
	if !errors.Is(err, errRunLeaseAuthorityLapsed) {
		t.Fatalf("renewal error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("renewal exceeded authority window: %s", elapsed)
	}
	if controlPlane.calls < 1 {
		t.Fatal("Control Plane renewal was not attempted")
	}
}

func TestRenewRunLeaseAuthorityDoesNotRetryGuestRejection(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	mounts := &rejectingRenewalMounts{}
	_, _, err := renewRunLeaseAuthority(
		context.Background(),
		staticRenewalControlPlane{response: testRunLeaseRenewResponse(renewed)},
		mounts,
		previous,
		&workspacev0.WorkspaceRunAuthority{},
	)
	if err == nil || err.Error() != "guest rejected renewal" {
		t.Fatalf("renewal error = %v", err)
	}
	if mounts.calls != 1 {
		t.Fatalf("guest renewal calls = %d", mounts.calls)
	}
}

func TestGuestRunLeaseTaskFrozenCheckpointRenewsOnlyControlPlaneAuthority(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	trace := &runLeaseTrace{}
	mounts := &rejectingRenewalMounts{}
	task := &guestRunLeaseTask{
		mounts:           mounts,
		controlPlane:     &testRunLeaseControlPlane{trace: trace, renewed: testRunLeaseRenewResponse(renewed)},
		lease:            previous,
		checkpointFrozen: true,
	}

	got, err := task.RenewRunLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !equalRunLeaseAssignment(got.Previous, previous) ||
		!equalRunLeaseAssignment(got.Lease, renewed) ||
		!equalRunLeaseAssignment(task.lease, renewed) {
		t.Fatalf("renewal = %+v, task Lease = %+v", got, task.lease)
	}
	if mounts.calls != 0 {
		t.Fatalf("frozen checkpoint contacted guest Workspace authority %d times", mounts.calls)
	}
	if len(trace.calls) != 1 || trace.calls[0] != "renew" {
		t.Fatalf("renewal trace = %v, want control-only renew", trace.calls)
	}
}

func TestGuestRunLeaseTaskSerializesRenewalWithCheckpointFreeze(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(time.Minute))
	first := previous
	first.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	second := first
	second.ExpiresAt = first.ExpiresAt.Add(time.Minute)
	controlPlane := &sequencedRenewalControlPlane{responses: []workerapi.RunLeaseRenewResponse{
		testRunLeaseRenewResponse(first), testRunLeaseRenewResponse(second),
	}}
	mounts := &blockingRenewalMounts{
		started: make(chan struct{}), release: make(chan struct{}),
	}
	task := &guestRunLeaseTask{
		mounts: mounts, controlPlane: controlPlane, lease: previous,
		authority: &workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				ExpiresAtUnixNano: previous.ExpiresAt.UnixNano(),
			},
		},
	}
	firstRenewal := make(chan error, 1)
	go func() {
		_, err := task.RenewRunLease(context.Background())
		firstRenewal <- err
	}()
	<-mounts.started

	stream := &signalingCheckpointStream{
		checkpointStream: newCheckpointStream(t, nil, &runv0.CheckpointPauseReady{
			RunWaitId: "run-wait-id-1", CheckpointId: "checkpoint-1",
		}),
		wrote: make(chan struct{}),
	}
	frozen := make(chan struct{})
	checkpointDone := make(chan error, 1)
	go func() {
		_, err := runtimeCheckpointer{
			stream: stream, freezeGate: &task.renewalGate,
			onFrozen: func() {
				task.markCheckpointFrozen()
				close(frozen)
			},
		}.suspendGuestForCheckpoint(context.Background(), CheckpointRequest{
			RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1",
		})
		checkpointDone <- err
	}()
	<-stream.wrote
	select {
	case <-frozen:
		t.Fatal("checkpoint froze before the in-flight guest renewal completed")
	default:
	}
	close(mounts.release)
	requirePromptResult(t, firstRenewal, "pre-freeze renewal")
	requirePromptResult(t, checkpointDone, "checkpoint freeze")
	select {
	case <-frozen:
	default:
		t.Fatal("checkpoint did not publish frozen authority")
	}

	if _, err := task.RenewRunLease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mounts.calls != 1 {
		t.Fatalf("guest authority renewals = %d, want only pre-freeze renewal", mounts.calls)
	}
	if controlPlane.calls != 2 || !equalRunLeaseAssignment(task.lease, second) {
		t.Fatalf("Control Plane renewals = %d, task Lease = %+v", controlPlane.calls, task.lease)
	}
}

func requirePromptResult(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s did not complete", operation)
	}
}

type signalingCheckpointStream struct {
	*checkpointStream
	once  sync.Once
	wrote chan struct{}
}

func (stream *signalingCheckpointStream) Write(body []byte) (int, error) {
	stream.once.Do(func() { close(stream.wrote) })
	return stream.checkpointStream.Write(body)
}

type sequencedRenewalControlPlane struct {
	RunLeaseControlPlane
	responses []workerapi.RunLeaseRenewResponse
	calls     int
}

func (controlPlane *sequencedRenewalControlPlane) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	if controlPlane.calls >= len(controlPlane.responses) {
		return workerapi.RunLeaseRenewResponse{}, errors.New("unexpected renewal")
	}
	response := controlPlane.responses[controlPlane.calls]
	controlPlane.calls++
	return response, nil
}

type blockingRenewalMounts struct {
	WorkspaceMountSessionRegistry
	started chan struct{}
	release chan struct{}
	calls   int
}

func (mounts *blockingRenewalMounts) RenewWorkspaceAuthority(
	_ context.Context,
	request *workspacev0.RenewWorkspaceAuthorityRequest,
) (*workspacev0.WorkspaceAuthorityFence, error) {
	mounts.calls++
	close(mounts.started)
	<-mounts.release
	fence := proto.Clone(request.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
	fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	return fence, nil
}

type cancelingRenewalControlPlane struct {
	cancel   context.CancelFunc
	response workerapi.RunLeaseRenewResponse
}

type failingRenewalControlPlane struct {
	calls int
}

func (controlPlane *failingRenewalControlPlane) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	controlPlane.calls++
	return workerapi.RunLeaseRenewResponse{}, errors.New("control plane unavailable")
}

type staticRenewalControlPlane struct {
	response workerapi.RunLeaseRenewResponse
}

func (controlPlane staticRenewalControlPlane) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	return controlPlane.response, nil
}

type rejectingRenewalMounts struct {
	WorkspaceMountSessionRegistry
	calls int
}

func (mounts *rejectingRenewalMounts) RenewWorkspaceAuthority(
	context.Context,
	*workspacev0.RenewWorkspaceAuthorityRequest,
) (*workspacev0.WorkspaceAuthorityFence, error) {
	mounts.calls++
	return nil, errors.New("guest rejected renewal")
}

func (controlPlane cancelingRenewalControlPlane) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	controlPlane.cancel()
	return controlPlane.response, nil
}

type runLeaseTrace struct {
	calls []string
}

func (trace *runLeaseTrace) add(call string) {
	trace.calls = append(trace.calls, call)
}

type testRunLeaseTaskRunner struct {
	trace *runLeaseTrace
	task  RunLeaseTask
}

func (runner *testRunLeaseTaskRunner) StartRunLeaseTask(
	_ context.Context,
	claim *workerapi.RunLeaseClaimResponse,
	_ RunLeaseControlPlane,
) (RunLeaseTask, error) {
	runner.trace.add("start")
	if task, ok := runner.task.(*testRunLeaseTask); ok && claim != nil {
		task.previous = claim.Lease
	}
	return runner.task, nil
}

type testRunLeaseTask struct {
	trace           *runLeaseTrace
	result          RunLeaseTaskResult
	previous        workerapi.RunLeaseAssignment
	renewed         workerapi.RunLeaseAssignment
	beginFailures   int
	captureFailures int
}

func (task *testRunLeaseTask) Close() {}

func (task *testRunLeaseTask) Wait(context.Context) (RunLeaseTaskResult, error) {
	task.trace.add("wait")
	return task.result, nil
}

func (task *testRunLeaseTask) RenewRunLease(
	context.Context,
) (RunLeaseTaskRenewal, error) {
	task.trace.add("renew")
	return RunLeaseTaskRenewal{Previous: task.previous, Lease: task.renewed}, nil
}

func (task *testRunLeaseTask) BeginWorkspaceFinalization(
	_ context.Context,
	_ workerapi.RunLeaseAssignment,
	_ workerapi.RunLeaseAssignment,
	_ string,
	_ workerapi.RunFinalizationKind,
) error {
	task.trace.add("guest-begin")
	if task.beginFailures > 0 {
		task.beginFailures--
		return errors.New("transient guest begin failure")
	}
	return nil
}

func (task *testRunLeaseTask) CaptureWorkspace(context.Context) (workerapi.TaskWorkspaceCapture, error) {
	task.trace.add("capture")
	if task.captureFailures > 0 {
		task.captureFailures--
		return workerapi.TaskWorkspaceCapture{}, errors.New("transient capture failure")
	}
	return workerapi.TaskWorkspaceCapture{}, nil
}

func (task *testRunLeaseTask) ResetWorkspace(context.Context) (workerapi.TaskWorkspaceRollback, error) {
	task.trace.add("reset")
	return workerapi.TaskWorkspaceRollback{}, nil
}

type testRunLeaseControlPlane struct {
	trace          *runLeaseTrace
	claim          workerapi.RunLeaseClaimResponse
	renewed        workerapi.RunLeaseRenewResponse
	begin          workerapi.BeginRunFinalizationResponse
	completed      workerapi.CompleteTaskRequest
	completedActor workerapi.CompleteActorRequest

	beginFailures     int
	completeFailures  int
	beginOperationIDs []string
}

func (controlPlane *testRunLeaseControlPlane) ClaimRunLease(
	context.Context,
	workerapi.RunLeaseWork,
) (workerapi.RunLeaseClaimResponse, error) {
	controlPlane.trace.add("claim")
	return controlPlane.claim, nil
}

func (controlPlane *testRunLeaseControlPlane) AcknowledgeRunStart(
	context.Context,
	workerapi.RunStartRequest,
) (workerapi.RunStartResponse, error) {
	return workerapi.RunStartResponse{}, nil
}

func (controlPlane *testRunLeaseControlPlane) AcknowledgeRunEntrypoint(
	context.Context,
	workerapi.RunEntrypointRequest,
) error {
	return nil
}

func (controlPlane *testRunLeaseControlPlane) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	controlPlane.trace.add("renew")
	return controlPlane.renewed, nil
}

func (controlPlane *testRunLeaseControlPlane) BeginRunFinalization(
	_ context.Context,
	request workerapi.BeginRunFinalizationRequest,
) (workerapi.BeginRunFinalizationResponse, error) {
	controlPlane.trace.add("begin")
	controlPlane.beginOperationIDs = append(controlPlane.beginOperationIDs, request.OperationID)
	if controlPlane.beginFailures > 0 {
		controlPlane.beginFailures--
		return workerapi.BeginRunFinalizationResponse{}, errors.New("transient begin failure")
	}
	controlPlane.begin.OperationID = request.OperationID
	return controlPlane.begin, nil
}

func (controlPlane *testRunLeaseControlPlane) CompleteTask(
	_ context.Context,
	request workerapi.CompleteTaskRequest,
) error {
	controlPlane.trace.add("complete")
	if controlPlane.completeFailures > 0 {
		controlPlane.completeFailures--
		return errors.New("transient completion failure")
	}
	controlPlane.completed = request
	return nil
}

func (controlPlane *testRunLeaseControlPlane) CompleteActor(
	_ context.Context,
	request workerapi.CompleteActorRequest,
) error {
	controlPlane.trace.add("complete-actor")
	if controlPlane.completeFailures > 0 {
		controlPlane.completeFailures--
		return errors.New("transient actor completion failure")
	}
	controlPlane.completedActor = request
	return nil
}

func (controlPlane *testRunLeaseControlPlane) CommitActorTurn(
	context.Context,
	workerapi.CommitActorTurnRequest,
) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, errors.New("unexpected actor turn commit")
}

func (controlPlane *testRunLeaseControlPlane) SendRunActorInput(
	context.Context,
	workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	return workerapi.SendActorInputResponse{}, errors.New("unexpected actor input send")
}

func (controlPlane *testRunLeaseControlPlane) AppendActorOutput(
	context.Context,
	workerapi.AppendActorOutputRequest,
) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected actor output append")
}

func (controlPlane *testRunLeaseControlPlane) CreateRuntimeToken(
	context.Context,
	workerapi.CreateTokenRequest,
) (api.TokenResponse, error) {
	return api.TokenResponse{}, errors.New("unexpected token create")
}

func (controlPlane *testRunLeaseControlPlane) AppendRunLog(
	context.Context,
	workerapi.RunLeaseAssignment,
	workerapi.LogStream,
	uint64,
	[]byte,
) error {
	return nil
}

func testRunLeaseAssignment(expiresAt time.Time) workerapi.RunLeaseAssignment {
	return workerapi.RunLeaseAssignment{
		ID:            "019c10d5-a6f7-7af1-8f5f-000000000001",
		RunID:         "019c10d5-a6f7-7af1-8f5f-000000000002",
		AttemptNumber: 1, LeaseSequence: 1,
		ExpiresAt: expiresAt.UTC(),
	}
}

func testRunLeaseRenewResponse(
	lease workerapi.RunLeaseAssignment,
) workerapi.RunLeaseRenewResponse {
	return workerapi.RunLeaseRenewResponse{
		Lease: lease.Fence(), ExpiresAt: lease.ExpiresAt,
		BaseWorkspaceVersionID: lease.BaseWorkspaceVersionID,
	}
}

func testRunFinalizationResponse(
	lease workerapi.RunLeaseAssignment,
	kind workerapi.RunFinalizationKind,
) workerapi.BeginRunFinalizationResponse {
	return workerapi.BeginRunFinalizationResponse{
		Lease: lease.Fence(), ExpiresAt: lease.ExpiresAt,
		BaseWorkspaceVersionID: lease.BaseWorkspaceVersionID,
		Kind:                   kind,
	}
}
