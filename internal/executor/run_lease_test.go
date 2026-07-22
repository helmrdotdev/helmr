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
	"google.golang.org/protobuf/proto"
)

func TestExecutorCompletesSuccessfulRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseReceipt(time.Now().Add(time.Minute))
	renewed := lease
	renewed.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	frozen := renewed
	frozen.ExpiresAt = renewed.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: renewed,
		result: RunLeaseTaskResult{
			Outcome: api.WorkerTaskOutcome{Succeeded: &api.WorkerTaskSucceeded{
				Output: json.RawMessage(`{"ok":true}`),
			}},
			ProgramQuiesced: api.WorkerRunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
	}
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   api.WorkerRunLeaseClaimResponse{Lease: lease},
		renewed: api.WorkerRunLeaseRenewResponse{Lease: renewed},
		begin: api.WorkerBeginRunFinalizationResponse{
			Lease: frozen, Kind: api.WorkerRunFinalizationCapture,
		},
	}
	runner := &testRunLeaseTaskRunner{trace: trace, task: task}
	executor := Executor{RunLeases: control, RunLeaseTasks: runner}

	err := executor.ExecuteRunLease(context.Background(), api.WorkerRunLeaseWork{
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

func TestExecutorRollsBackFailedRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseReceipt(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
		result: RunLeaseTaskResult{
			Outcome: api.WorkerTaskOutcome{Failed: &api.WorkerTaskFailure{Message: "failed"}},
			ProgramQuiesced: api.WorkerRunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
	}
	control := &testRunLeaseControl{
		trace:   trace,
		claim:   api.WorkerRunLeaseClaimResponse{Lease: lease},
		renewed: api.WorkerRunLeaseRenewResponse{Lease: lease},
		begin: api.WorkerBeginRunFinalizationResponse{
			Lease: frozen, Kind: api.WorkerRunFinalizationReset,
		},
	}
	executor := Executor{
		RunLeases:     control,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}

	err := executor.ExecuteRunLease(context.Background(), api.WorkerRunLeaseWork{
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

func TestExecutorReplaysFinalizationWithStableAuthority(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseReceipt(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:   trace,
		renewed: lease,
		result: RunLeaseTaskResult{
			Outcome: api.WorkerTaskOutcome{Succeeded: &api.WorkerTaskSucceeded{
				Output: json.RawMessage(`null`),
			}},
			ProgramQuiesced: api.WorkerRunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
		beginFailures:   1,
		captureFailures: 1,
	}
	control := &testRunLeaseControl{
		trace:            trace,
		claim:            api.WorkerRunLeaseClaimResponse{Lease: lease},
		begin:            api.WorkerBeginRunFinalizationResponse{Lease: frozen, Kind: api.WorkerRunFinalizationCapture},
		beginFailures:    1,
		completeFailures: 1,
	}
	executor := Executor{
		RunLeases:     control,
		RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task},
	}
	if err := executor.ExecuteRunLease(context.Background(), api.WorkerRunLeaseWork{
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

func TestRenewRunLeaseAuthorityInstallsCommittedRenewalAfterCallerCancellation(t *testing.T) {
	previous := testRunLeaseReceipt(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	control := cancelingRenewalControl{
		cancel:   cancel,
		response: api.WorkerRunLeaseRenewResponse{Lease: renewed},
	}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(api.WorkerWorkspaceMount{
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
	if !equalRunLeaseReceipt(got, renewed) ||
		fence.GetExpiresAtUnixNano() != renewed.ExpiresAt.UnixNano() {
		t.Fatalf("renewal = (%+v, %+v)", got, fence)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestRenewRunLeaseAuthorityStopsAtGuestAcknowledgedExpiry(t *testing.T) {
	previous := testRunLeaseReceipt(time.Now().Add(250 * time.Millisecond))
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
	previous := testRunLeaseReceipt(time.Now().Add(time.Minute))
	renewed := previous
	renewed.ExpiresAt = previous.ExpiresAt.Add(time.Minute)
	mounts := &rejectingRenewalMounts{}
	_, _, err := renewRunLeaseAuthority(
		context.Background(),
		staticRenewalControl{response: api.WorkerRunLeaseRenewResponse{Lease: renewed}},
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
	response api.WorkerRunLeaseRenewResponse
}

type failingRenewalControl struct {
	calls int
}

func (control *failingRenewalControl) RenewRunLease(
	context.Context,
	api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseRenewResponse, error) {
	control.calls++
	return api.WorkerRunLeaseRenewResponse{}, errors.New("Control unavailable")
}

type staticRenewalControl struct {
	response api.WorkerRunLeaseRenewResponse
}

func (control staticRenewalControl) RenewRunLease(
	context.Context,
	api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseRenewResponse, error) {
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
	api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseRenewResponse, error) {
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
	_ *api.WorkerRunLeaseClaimResponse,
	_ RunLeaseControl,
) (RunLeaseTask, error) {
	runner.trace.add("start")
	return runner.task, nil
}

type testRunLeaseTask struct {
	trace           *runLeaseTrace
	result          RunLeaseTaskResult
	renewed         api.WorkerRunLeaseReceipt
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
) (api.WorkerRunLeaseReceipt, error) {
	task.trace.add("renew")
	return task.renewed, nil
}

func (task *testRunLeaseTask) BeginWorkspaceFinalization(
	_ context.Context,
	_ api.WorkerRunLeaseReceipt,
	_ api.WorkerRunLeaseReceipt,
	_ string,
	_ api.WorkerRunFinalizationKind,
) error {
	task.trace.add("guest-begin")
	if task.beginFailures > 0 {
		task.beginFailures--
		return errors.New("transient guest begin failure")
	}
	return nil
}

func (task *testRunLeaseTask) CaptureWorkspace(context.Context) (api.WorkerTaskWorkspaceCapture, error) {
	task.trace.add("capture")
	if task.captureFailures > 0 {
		task.captureFailures--
		return api.WorkerTaskWorkspaceCapture{}, errors.New("transient capture failure")
	}
	return api.WorkerTaskWorkspaceCapture{}, nil
}

func (task *testRunLeaseTask) ResetWorkspace(context.Context) (api.WorkerTaskWorkspaceRollback, error) {
	task.trace.add("reset")
	return api.WorkerTaskWorkspaceRollback{}, nil
}

type testRunLeaseControl struct {
	trace     *runLeaseTrace
	claim     api.WorkerRunLeaseClaimResponse
	renewed   api.WorkerRunLeaseRenewResponse
	begin     api.WorkerBeginRunFinalizationResponse
	completed api.WorkerCompleteTaskRequest

	beginFailures     int
	completeFailures  int
	beginOperationIDs []string
}

func (control *testRunLeaseControl) ClaimRunLease(
	context.Context,
	api.WorkerRunLeaseWork,
) (api.WorkerRunLeaseClaimResponse, error) {
	control.trace.add("claim")
	return control.claim, nil
}

func (control *testRunLeaseControl) AcknowledgeRunStart(
	context.Context,
	api.WorkerRunStartRequest,
) (api.WorkerRunStartResponse, error) {
	return api.WorkerRunStartResponse{}, nil
}

func (control *testRunLeaseControl) AcknowledgeRunEntrypoint(
	context.Context,
	api.WorkerRunEntrypointRequest,
) error {
	return nil
}

func (control *testRunLeaseControl) RenewRunLease(
	context.Context,
	api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseRenewResponse, error) {
	control.trace.add("renew")
	return control.renewed, nil
}

func (control *testRunLeaseControl) BeginRunFinalization(
	_ context.Context,
	request api.WorkerBeginRunFinalizationRequest,
) (api.WorkerBeginRunFinalizationResponse, error) {
	control.trace.add("begin")
	control.beginOperationIDs = append(control.beginOperationIDs, request.OperationID)
	if control.beginFailures > 0 {
		control.beginFailures--
		return api.WorkerBeginRunFinalizationResponse{}, errors.New("transient begin failure")
	}
	control.begin.OperationID = request.OperationID
	return control.begin, nil
}

func (control *testRunLeaseControl) CompleteTask(
	_ context.Context,
	request api.WorkerCompleteTaskRequest,
) error {
	control.trace.add("complete")
	if control.completeFailures > 0 {
		control.completeFailures--
		return errors.New("transient completion failure")
	}
	control.completed = request
	return nil
}

func (control *testRunLeaseControl) AppendRunLog(
	context.Context,
	api.WorkerRunLeaseReceipt,
	api.WorkerLogStream,
	uint64,
	[]byte,
) error {
	return nil
}

func testRunLeaseReceipt(expiresAt time.Time) api.WorkerRunLeaseReceipt {
	return api.WorkerRunLeaseReceipt{
		ID:            "00000000-0000-0000-0000-000000000001",
		RunID:         "00000000-0000-0000-0000-000000000002",
		AttemptNumber: 1, LeaseSequence: 1,
		ExpiresAt: expiresAt.UTC(),
	}
}
