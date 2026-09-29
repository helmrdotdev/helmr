package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestExecutorCompletesSuccessfulRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	renewed := lease
	renewed.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	frozen := renewed
	frozen.ExpiresAt = renewed.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:    trace,
		renewErr: errors.New("program claim is not active"),
		renewed:  renewed,
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
		begin:   testRunFinalizationResponse(frozen),
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
		"claim", "start", "wait", "begin",
		"complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completed.OperationID != controlPlane.beginOperationIDs[0] ||
		controlPlane.completed.Outcome.Succeeded == nil {
		t.Fatalf("completion = %+v", controlPlane.completed)
	}
}

func TestExecutorCompletesFailedRunLeaseTask(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:    trace,
		renewErr: errors.New("program claim is not active"),
		renewed:  lease,
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
		begin:   testRunFinalizationResponse(frozen),
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
		"claim", "start", "wait", "begin", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completed.OperationID != controlPlane.beginOperationIDs[0] ||
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
		trace:    trace,
		renewErr: errors.New("program claim is not active"),
		renewed:  lease,
		result: RunLeaseTaskResult{
			ActorOutcome: &workerapi.ActorOutcome{
				RunGeneration: 4,
				Succeeded:     &workerapi.ActorSucceeded{},
			},
			ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID},
		},
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:   trace,
		claim:   workerapi.RunLeaseClaimResponse{Lease: lease},
		renewed: testRunLeaseRenewResponse(lease),
		begin:   testRunFinalizationResponse(frozen),
	}
	executor := Executor{RunLeases: controlPlane, RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task}}
	if err := executor.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.calls, []string{"claim", "start", "wait", "begin", "complete-actor"}) {
		t.Fatalf("calls = %v", trace.calls)
	}
	if controlPlane.completedActor.Outcome.Succeeded == nil || controlPlane.completedActor.Outcome.RunGeneration != 4 || controlPlane.completedActor.OperationID != controlPlane.beginOperationIDs[0] {
		t.Fatalf("Actor completion = %+v", controlPlane.completedActor)
	}
}

func TestExecutorReplaysFinalizationWithStableAuthority(t *testing.T) {
	trace := &runLeaseTrace{}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	frozen := lease
	frozen.ExpiresAt = lease.ExpiresAt.Add(20 * time.Minute)
	task := &testRunLeaseTask{
		trace:    trace,
		renewErr: errors.New("program claim is not active"),
		renewed:  lease,
		result: RunLeaseTaskResult{
			Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
				Output: json.RawMessage(`null`),
			}},
			ProgramQuiesced: workerapi.RunQuiescenceProof{
				RunID: lease.RunID, AttemptNumber: lease.AttemptNumber,
				RunLeaseID: lease.ID,
			},
		},
	}
	controlPlane := &testRunLeaseControlPlane{
		trace:            trace,
		claim:            workerapi.RunLeaseClaimResponse{Lease: lease},
		begin:            testRunFinalizationResponse(frozen),
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
		"claim", "start", "wait", "begin", "begin", "complete", "complete",
	}) {
		t.Fatalf("calls = %v", trace.calls)
	}
}

func TestExecutorRenewalAcceptsCommittedActorComputerFrontier(t *testing.T) {
	trace := &runLeaseTrace{}
	current := testRunLeaseAssignment(time.Now().Add(time.Minute))
	current.BaseComputerDiskVersionID = "version-1"
	previous := current
	previous.BaseComputerDiskVersionID = "version-2"
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
	registry := newTestMounts()
	_, unregister := registry.add(workerapi.ComputerInstanceAssignment{
		ComputerID: "computer-1", ComputerInstanceID: "runtime-1",
		WriterGeneration: 4, Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"},
	}, fakeGuestSession{}, "channel-1")
	defer unregister()
	authority := &computerv0.ComputerRunAuthority{
		Fence: &computerv0.ComputerAuthorityFence{
			ComputerInstanceId: "runtime-1", ComputerId: "computer-1", WriterGeneration: 4,
			RunId: previous.RunID, ExpiresAtUnixNano: previous.ExpiresAt.UnixNano(),
			BaseComputerDiskVersionId: "version-1",
		},
		ChannelToken: "channel-1",
	}
	got, fence, err := renewRunLeaseAuthority(ctx, controlPlane, registry, previous, authority)
	if err != nil {
		t.Fatal(err)
	}
	if !equalRunLeaseAssignment(got, renewed) ||
		fence.GetExpiresAtUnixNano() != renewed.ExpiresAt.UnixNano() {
		t.Fatalf("renewal = (%+v, %+v)", got, fence)
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
		&computerv0.ComputerRunAuthority{},
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
		&computerv0.ComputerRunAuthority{},
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
		controlPlane:     testControlPlane(t, &testRunLeaseControlPlane{trace: trace, renewed: testRunLeaseRenewResponse(renewed)}),
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
		t.Fatalf("frozen checkpoint contacted guest Computer authority %d times", mounts.calls)
	}
	if len(trace.calls) != 1 || trace.calls[0] != "renew" {
		t.Fatalf("renewal trace = %v, want control-only renew", trace.calls)
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
	MountRegistry
	calls int
}

func (mounts *rejectingRenewalMounts) RenewComputerAuthority(
	context.Context,
	*computerv0.RenewComputerAuthorityRequest,
) (*computerv0.ComputerAuthorityFence, error) {
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
) (RunLeaseTask, error) {
	runner.trace.add("start")
	if task, ok := runner.task.(*testRunLeaseTask); ok && claim != nil {
		task.previous = claim.Lease
	}
	return runner.task, nil
}

type testRunLeaseTask struct {
	waitErr  error
	renewErr error
	trace    *runLeaseTrace
	result   RunLeaseTaskResult
	previous workerapi.RunLeaseAssignment
	renewed  workerapi.RunLeaseAssignment
}

func (task *testRunLeaseTask) Close() {}

func (task *testRunLeaseTask) Wait(context.Context) (RunLeaseTaskResult, error) {
	task.trace.add("wait")
	return task.result, task.waitErr
}

func (task *testRunLeaseTask) RenewRunLease(
	context.Context,
) (RunLeaseTaskRenewal, error) {
	task.trace.add("renew")
	if task.renewErr != nil {
		return RunLeaseTaskRenewal{}, task.renewErr
	}
	return RunLeaseTaskRenewal{Previous: task.previous, Lease: task.renewed}, nil
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
		BaseComputerDiskVersionID: lease.BaseComputerDiskVersionID,
	}
}

func testRunFinalizationResponse(
	lease workerapi.RunLeaseAssignment,
) workerapi.BeginRunFinalizationResponse {
	return workerapi.BeginRunFinalizationResponse{
		Lease: lease.Fence(), ExpiresAt: lease.ExpiresAt,
	}
}

func TestExecutorPreservesCheckpointReleaseFailureAfterDetachment(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			trace := &runLeaseTrace{}
			lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
			releaseErr := errors.New("physical stop uncertain")
			waitErr := ErrDetached
			if failed {
				waitErr = errors.Join(ErrDetached, &SourceReleaseError{Err: releaseErr})
			}
			task := &testRunLeaseTask{trace: trace, waitErr: waitErr}
			client := &testRunLeaseControlPlane{trace: trace, claim: workerapi.RunLeaseClaimResponse{Lease: lease}}
			e := Executor{RunLeases: client, RunLeaseTasks: &testRunLeaseTaskRunner{trace: trace, task: task}}
			err := e.ExecuteRunLease(context.Background(), workerapi.RunLeaseWork{LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence})
			if failed && !errors.Is(err, releaseErr) {
				t.Fatalf("lost release failure: %v", err)
			}
			if !failed && err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(trace.calls, []string{"claim", "start", "wait"}) {
				t.Fatalf("finalized detached run: %v", trace.calls)
			}
		})
	}
}

// A released Guest claim can reject renewal before its terminal proof reaches
// the Worker. The reader must retain that proof for Control Plane settlement.
func TestRunLeaseRenewalRaceWithQuiescence(t *testing.T) {
	for _, scenario := range []string{"completed", "reader-error", "expired", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				task := &quiescingRenewalTask{renewed: make(chan struct{}), scenario: scenario, cancel: cancel}
				lease := testRunLeaseAssignment(time.Now().Add(90 * time.Millisecond))
				task.result = RunLeaseTaskResult{Outcome: workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "task failed"}}, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID}}
				result, current, err := (Executor{}).awaitRunLeaseTask(ctx, task, lease)
				if scenario == "completed" {
					if err != nil || result.Outcome.Failed == nil || result.ProgramQuiesced != task.result.ProgramQuiesced || current.ID != lease.ID {
						t.Fatalf("result=%+v current=%+v err=%v", result, current, err)
					}
				} else if err == nil || result.Outcome.Failed != nil {
					t.Fatalf("accepted incomplete result: %+v, %v", result, err)
				}
			})
		})
	}
}

type quiescingRenewalTask struct {
	renewed  chan struct{}
	scenario string
	cancel   context.CancelFunc
	result   RunLeaseTaskResult
}

func (*quiescingRenewalTask) Close() {}
func (task *quiescingRenewalTask) Wait(ctx context.Context) (RunLeaseTaskResult, error) {
	select {
	case <-task.renewed:
	case <-ctx.Done():
		return RunLeaseTaskResult{}, ctx.Err()
	}
	switch task.scenario {
	case "completed":
		return task.result, nil
	case "reader-error":
		return RunLeaseTaskResult{}, errors.New("invalid quiescence proof")
	default:
		<-ctx.Done()
		return RunLeaseTaskResult{}, ctx.Err()
	}
}
func (task *quiescingRenewalTask) RenewRunLease(context.Context) (RunLeaseTaskRenewal, error) {
	close(task.renewed)
	if task.scenario == "cancelled" {
		task.cancel()
	}
	return RunLeaseTaskRenewal{}, errors.New("program claim is not active")
}
