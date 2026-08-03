package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/frameio"
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
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(renewed),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
	}
	runner := &testRunLeaseTaskRunner{trace: trace, task: task}
	executor := Executor{RunLeases: control, RunLeaseTasks: runner}

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
	if control.completed.Workspace.Captured == nil ||
		control.completed.Workspace.RolledBack != nil ||
		control.completed.Outcome.Succeeded == nil {
		t.Fatalf("completion = %+v", control.completed)
	}
}

func TestExecutorCompletesSuccessfulTaskHandoff(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
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
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin: workerapi.BeginRunFinalizationResponse{
			Lease: frozen.Fence(), ExpiresAt: frozen.ExpiresAt,
			BaseWorkspaceVersionID: frozen.BaseWorkspaceVersionID,
			Kind:                   workerapi.RunFinalizationCapture,
			Handoff: &workerapi.RunFinalizationHandoff{
				ParentRunID: "parent-run", ParentAttemptNumber: 1,
				RunWaitID: "wait", SuspendCheckpointID: "suspend",
				ResumeAttachID: "attach", CorrelationID: "correlation",
			},
		},
	}
	executor := Executor{
		RunLeases:     control,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}

	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{
		LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{
		"claim", "start", "wait", "renew", "begin", "guest-begin", "capture",
		"handoff-checkpoint", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if control.completed.Handoff == nil ||
		control.completed.Handoff.CheckpointID == "" ||
		control.completed.Workspace.Captured == nil ||
		control.completed.Outcome.Succeeded == nil {
		t.Fatalf("completion = %+v", control.completed)
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
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationReset),
	}
	executor := Executor{
		RunLeases:     control,
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
	if control.completed.Workspace.Captured != nil ||
		control.completed.Workspace.RolledBack == nil ||
		control.completed.Outcome.Failed == nil {
		t.Fatalf("completion = %+v", control.completed)
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
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin:   testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
	}
	executor := Executor{RunLeases: control, RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task}}
	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{"claim", "start", "wait", "renew", "begin", "guest-begin", "capture", "complete-actor"}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if control.completedActor.Outcome.Succeeded == nil || control.completedActor.Outcome.TerminalInputSequence != 4 || control.completedActor.Workspace.Captured == nil {
		t.Fatalf("Actor completion = %+v", control.completedActor)
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
	control := &testRunLeaseControl{
		trace:            trace,
		claim:            workerapi.RunLeaseClaimResponse{Lease: lease},
		begin:            testRunFinalizationResponse(frozen, workerapi.RunFinalizationCapture),
		beginFailures:    1,
		completeFailures: 1,
	}
	executor := Executor{
		RunLeases:     control,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}
	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{
		LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if len(control.beginOperationIDs) != 2 ||
		control.beginOperationIDs[0] != control.beginOperationIDs[1] {
		t.Fatalf("begin operation IDs = %v", control.beginOperationIDs)
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
	control := cancelingRenewalControl{
		cancel:   cancel,
		response: testRunLeaseRenewResponse(renewed),
	}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", RuntimeInstanceID: "runtime-1",
		FencingGeneration: 4, BaseVersionID: "version-1",
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
	got, fence, err := renewRunLeaseAuthority(ctx, control, registry, previous, authority)
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
	control := &failingRenewalControl{}
	started := time.Now()
	_, _, err := renewRunLeaseAuthority(
		context.Background(),
		control,
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
	if control.calls < 1 {
		t.Fatal("Control renewal was not attempted")
	}
}

func TestRenewRunLeaseAuthorityDoesNotRetryGuestRejection(t *testing.T) {
	previous := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	mounts := &rejectingRenewalMounts{}
	_, _, err := renewRunLeaseAuthority(
		context.Background(),
		staticRenewalControl{response: testRunLeaseRenewResponse(renewed)},
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

type cancelingRenewalControl struct {
	cancel   context.CancelFunc
	response workerapi.RunLeaseRenewResponse
}

type failingRenewalControl struct {
	calls int
}

func (control *failingRenewalControl) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	control.calls++
	return workerapi.RunLeaseRenewResponse{}, errors.New("Control unavailable")
}

type staticRenewalControl struct {
	response workerapi.RunLeaseRenewResponse
}

func (control staticRenewalControl) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	return control.response, nil
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

func (control cancelingRenewalControl) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	control.cancel()
	return control.response, nil
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
	_ RunLeaseControl,
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

func (task *testRunLeaseTask) CreateHandoffCheckpoint(
	context.Context,
	workerapi.RunFinalizationHandoff,
	string,
	workerapi.TaskWorkspaceCapture,
) (workerapi.CheckpointManifest, error) {
	task.trace.add("handoff-checkpoint")
	return workerapi.CheckpointManifest{}, nil
}

func (task *testRunLeaseTask) ResetWorkspace(context.Context) (workerapi.TaskWorkspaceRollback, error) {
	task.trace.add("reset")
	return workerapi.TaskWorkspaceRollback{}, nil
}

type testRunLeaseControl struct {
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

func (control *testRunLeaseControl) ClaimRunLease(
	context.Context,
	workerapi.RunLeaseWork,
) (workerapi.RunLeaseClaimResponse, error) {
	control.trace.add("claim")
	return control.claim, nil
}

func (control *testRunLeaseControl) AcknowledgeRunStart(
	context.Context,
	workerapi.RunStartRequest,
) (workerapi.RunStartResponse, error) {
	return workerapi.RunStartResponse{}, nil
}

func (control *testRunLeaseControl) AcknowledgeRunEntrypoint(
	context.Context,
	workerapi.RunEntrypointRequest,
) error {
	return nil
}

func (control *testRunLeaseControl) RenewRunLease(
	context.Context,
	workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	control.trace.add("renew")
	return control.renewed, nil
}

func (control *testRunLeaseControl) BeginRunFinalization(
	_ context.Context,
	request workerapi.BeginRunFinalizationRequest,
) (workerapi.BeginRunFinalizationResponse, error) {
	control.trace.add("begin")
	control.beginOperationIDs = append(control.beginOperationIDs, request.OperationID)
	if control.beginFailures > 0 {
		control.beginFailures--
		return workerapi.BeginRunFinalizationResponse{}, errors.New("transient begin failure")
	}
	control.begin.OperationID = request.OperationID
	return control.begin, nil
}

func (control *testRunLeaseControl) CompleteTask(
	_ context.Context,
	request workerapi.CompleteTaskRequest,
) error {
	control.trace.add("complete")
	if control.completeFailures > 0 {
		control.completeFailures--
		return errors.New("transient completion failure")
	}
	control.completed = request
	return nil
}

func (control *testRunLeaseControl) CompleteActor(
	_ context.Context,
	request workerapi.CompleteActorRequest,
) error {
	control.trace.add("complete-actor")
	if control.completeFailures > 0 {
		control.completeFailures--
		return errors.New("transient Actor completion failure")
	}
	control.completedActor = request
	return nil
}

func (control *testRunLeaseControl) CommitActorTurn(
	context.Context,
	workerapi.CommitActorTurnRequest,
) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, errors.New("unexpected Actor turn commit")
}

func (control *testRunLeaseControl) SendRunActorInput(
	context.Context,
	workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	return workerapi.SendActorInputResponse{}, errors.New("unexpected Actor input send")
}

func (control *testRunLeaseControl) AppendActorOutput(
	context.Context,
	workerapi.AppendActorOutputRequest,
) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected Actor output append")
}

func (control *testRunLeaseControl) CreateRuntimeToken(
	context.Context,
	workerapi.CreateTokenRequest,
) (api.TokenResponse, error) {
	return api.TokenResponse{}, errors.New("unexpected Token create")
}

func (control *testRunLeaseControl) AppendRunLog(
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
