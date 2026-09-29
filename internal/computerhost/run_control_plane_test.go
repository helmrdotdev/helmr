package computerhost_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The Run-side integration tests in this package drive executor only through
// its exported operations; this is their copy of executor's Control Plane
// test assembly.

// testControlPlane assembles a complete executor.ControlPlane from test fakes. Each
// capability is taken from the first fake implementing it; every remaining
// capability fails the test when called.
func testControlPlane(t testing.TB, fakes ...any) executor.ControlPlane {
	t.Helper()
	unexpected := unexpectedControlPlane{calls: &unexpectedControlPlaneCalls{}}
	t.Cleanup(func() {
		if calls := unexpected.calls.snapshot(); len(calls) != 0 {
			t.Errorf("unexpected Control Plane calls %v", calls)
		}
	})
	return executor.ControlPlane{
		Leases:        testCapability[executor.RunLeaseControlPlane](unexpected, fakes),
		Waits:         testCapability[executor.RunWaitClient](unexpected, fakes),
		Observability: testCapability[executor.RunObservabilityControlPlane](unexpected, fakes),
		Sessions: testSessionControlPlane{
			SessionExecutionControlPlane: testCapability[executor.SessionExecutionControlPlane](unexpected, fakes),
			SessionReferenceControlPlane: testCapability[executor.SessionReferenceControlPlane](unexpected, fakes),
			SessionSubmitControlPlane:    testCapability[executor.SessionSubmitControlPlane](unexpected, fakes),
		},
		Actors:    testCapability[executor.ActorRuntimeControlPlane](unexpected, fakes),
		Computers: testCapability[executor.ComputerRuntimeControlPlane](unexpected, fakes),
		Children:  testCapability[executor.ChildTaskControlPlane](unexpected, fakes),
	}
}

func testCapability[Capability any](unexpected Capability, fakes []any) Capability {
	for _, fake := range fakes {
		if capability, ok := fake.(Capability); ok {
			return capability
		}
	}
	return unexpected
}

type testSessionControlPlane struct {
	executor.SessionExecutionControlPlane
	executor.SessionReferenceControlPlane
	executor.SessionSubmitControlPlane
}

// unexpectedControlPlane records calls rather than failing the test directly,
// because a call may arrive from a goroutine after the test has returned. The
// owning test reports recorded calls during cleanup.
type unexpectedControlPlane struct {
	calls *unexpectedControlPlaneCalls
}

type unexpectedControlPlaneCalls struct {
	mu    sync.Mutex
	names []string
}

func (c *unexpectedControlPlaneCalls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

func (u unexpectedControlPlane) fail(call string) error {
	u.calls.mu.Lock()
	u.calls.names = append(u.calls.names, call)
	u.calls.mu.Unlock()
	return errors.New("unexpected Control Plane call " + call)
}

func (u unexpectedControlPlane) ClaimRunLease(context.Context, workerapi.RunLeaseWork) (workerapi.RunLeaseClaimResponse, error) {
	return workerapi.RunLeaseClaimResponse{}, u.fail("ClaimRunLease")
}

func (u unexpectedControlPlane) AcknowledgeRunStart(context.Context, workerapi.RunStartRequest) (workerapi.RunStartResponse, error) {
	return workerapi.RunStartResponse{}, u.fail("AcknowledgeRunStart")
}

func (u unexpectedControlPlane) AcknowledgeRunEntrypoint(context.Context, workerapi.RunEntrypointRequest) error {
	return u.fail("AcknowledgeRunEntrypoint")
}

func (u unexpectedControlPlane) RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error) {
	return workerapi.RunLeaseRenewResponse{}, u.fail("RenewRunLease")
}

func (u unexpectedControlPlane) BeginRunFinalization(context.Context, workerapi.BeginRunFinalizationRequest) (workerapi.BeginRunFinalizationResponse, error) {
	return workerapi.BeginRunFinalizationResponse{}, u.fail("BeginRunFinalization")
}

func (u unexpectedControlPlane) CompleteTask(context.Context, workerapi.CompleteTaskRequest) error {
	return u.fail("CompleteTask")
}

func (u unexpectedControlPlane) CompleteActor(context.Context, workerapi.CompleteActorRequest) error {
	return u.fail("CompleteActor")
}

func (u unexpectedControlPlane) CommitActorTurn(context.Context, workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, u.fail("CommitActorTurn")
}

func (u unexpectedControlPlane) CreateRuntimeToken(context.Context, workerapi.CreateTokenRequest) (api.TokenResponse, error) {
	return api.TokenResponse{}, u.fail("CreateRuntimeToken")
}

func (u unexpectedControlPlane) AppendRunLog(context.Context, workerapi.RunLeaseAssignment, workerapi.LogStream, uint64, []byte) error {
	return u.fail("AppendRunLog")
}

func (u unexpectedControlPlane) CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	return workerapi.CreateRunWaitResponse{}, u.fail("CreateRunWait")
}

func (u unexpectedControlPlane) PollRunWait(context.Context, workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	return workerapi.RunWaitPollResponse{}, u.fail("PollRunWait")
}

func (u unexpectedControlPlane) AcknowledgeRunWaitResume(context.Context, workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	return workerapi.RunWaitResumeAckResponse{}, u.fail("AcknowledgeRunWaitResume")
}

func (u unexpectedControlPlane) UpdateRunMetadata(context.Context, workerapi.UpdateRunMetadataRequest) error {
	return u.fail("UpdateRunMetadata")
}

func (u unexpectedControlPlane) AppendStructuredRunLog(context.Context, workerapi.StructuredLogRequest) error {
	return u.fail("AppendStructuredRunLog")
}

func (u unexpectedControlPlane) WriteTurnOutput(context.Context, workerapi.WriteTurnOutputRequest) (workerapi.WriteOutputResponse, error) {
	return workerapi.WriteOutputResponse{}, u.fail("WriteTurnOutput")
}

func (u unexpectedControlPlane) WriteSessionOutput(context.Context, workerapi.WriteSessionOutputRequest) (workerapi.WriteOutputResponse, error) {
	return workerapi.WriteOutputResponse{}, u.fail("WriteSessionOutput")
}

func (u unexpectedControlPlane) TurnMessagesReady(context.Context, workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error) {
	return workerapi.TurnCommandResponse{}, u.fail("TurnMessagesReady")
}

func (u unexpectedControlPlane) BeginTurnSettlement(context.Context, workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error) {
	return workerapi.TurnCommandResponse{}, u.fail("BeginTurnSettlement")
}

func (u unexpectedControlPlane) ClaimTurnMessage(context.Context, workerapi.ClaimTurnMessageRequest) (workerapi.ClaimTurnMessageResponse, error) {
	return workerapi.ClaimTurnMessageResponse{}, u.fail("ClaimTurnMessage")
}

func (u unexpectedControlPlane) CompleteTurnMessage(context.Context, workerapi.CompleteTurnMessageRequest) (workerapi.TurnCommandResponse, error) {
	return workerapi.TurnCommandResponse{}, u.fail("CompleteTurnMessage")
}

func (u unexpectedControlPlane) ReadSessionControl(context.Context, workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error) {
	return workerapi.SessionControlResponse{}, u.fail("ReadSessionControl")
}

func (u unexpectedControlPlane) GetRunSessionTurn(context.Context, workerapi.TurnReferenceRequest) (workerapi.SessionTurnResponse, error) {
	return workerapi.SessionTurnResponse{}, u.fail("GetRunSessionTurn")
}

func (u unexpectedControlPlane) InterruptRunSessionTurn(context.Context, workerapi.InterruptSessionTurnRequest) (workerapi.InterruptSessionTurnResponse, error) {
	return workerapi.InterruptSessionTurnResponse{}, u.fail("InterruptRunSessionTurn")
}

func (u unexpectedControlPlane) ResumeRunSession(context.Context, workerapi.ResumeSessionRequest) (workerapi.ResumeSessionResponse, error) {
	return workerapi.ResumeSessionResponse{}, u.fail("ResumeRunSession")
}

func (u unexpectedControlPlane) SendRunSession(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	return workerapi.SubmitSessionDataResponse{}, u.fail("SendRunSession")
}

func (u unexpectedControlPlane) EnqueueRunSession(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	return workerapi.SubmitSessionDataResponse{}, u.fail("EnqueueRunSession")
}

func (u unexpectedControlPlane) SendRunTurnMessage(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	return workerapi.SubmitSessionDataResponse{}, u.fail("SendRunTurnMessage")
}

func (u unexpectedControlPlane) StartRunActor(context.Context, workerapi.StartActorRequest) (workerapi.StartActorResponse, error) {
	return workerapi.StartActorResponse{}, u.fail("StartRunActor")
}

func (u unexpectedControlPlane) GetRunSessionStatus(context.Context, workerapi.SessionReferenceRequest) (workerapi.SessionStatusResponse, error) {
	return workerapi.SessionStatusResponse{}, u.fail("GetRunSessionStatus")
}

func (u unexpectedControlPlane) CloseRunSession(context.Context, workerapi.CloseSessionRequest) (workerapi.CloseSessionResponse, error) {
	return workerapi.CloseSessionResponse{}, u.fail("CloseRunSession")
}

func (u unexpectedControlPlane) CancelRunSession(context.Context, workerapi.CancelSessionRequest) (workerapi.CancelSessionResponse, error) {
	return workerapi.CancelSessionResponse{}, u.fail("CancelRunSession")
}

func (u unexpectedControlPlane) ReadRunSessionEvents(context.Context, workerapi.ReadSessionEventsRequest) (workerapi.ReadSessionEventsResponse, error) {
	return workerapi.ReadSessionEventsResponse{}, u.fail("ReadRunSessionEvents")
}

func (u unexpectedControlPlane) CreateRunComputer(context.Context, workerapi.CreateComputerRequest) (workerapi.CreateComputerResponse, error) {
	return workerapi.CreateComputerResponse{}, u.fail("CreateRunComputer")
}

func (u unexpectedControlPlane) RetrieveRunComputer(context.Context, workerapi.RetrieveComputerRequest) (workerapi.RetrieveComputerResponse, error) {
	return workerapi.RetrieveComputerResponse{}, u.fail("RetrieveRunComputer")
}

func (u unexpectedControlPlane) ListRunComputerMembers(context.Context, workerapi.ComputerMembersRequest) (workerapi.ComputerMembersResponse, error) {
	return workerapi.ComputerMembersResponse{}, u.fail("ListRunComputerMembers")
}

func (u unexpectedControlPlane) DeleteRunComputer(context.Context, workerapi.DeleteComputerRequest) (workerapi.DeleteComputerResponse, error) {
	return workerapi.DeleteComputerResponse{}, u.fail("DeleteRunComputer")
}

func (u unexpectedControlPlane) InvokeChildTask(context.Context, workerapi.InvokeChildTaskRequest) (workerapi.InvokeChildTaskResponse, error) {
	return workerapi.InvokeChildTaskResponse{}, u.fail("InvokeChildTask")
}

var (
	_ executor.RunLeaseControlPlane         = unexpectedControlPlane{}
	_ executor.RunWaitClient                = unexpectedControlPlane{}
	_ executor.RunObservabilityControlPlane = unexpectedControlPlane{}
	_ executor.SessionControlPlane          = unexpectedControlPlane{}
	_ executor.ActorRuntimeControlPlane     = unexpectedControlPlane{}
	_ executor.ComputerRuntimeControlPlane  = unexpectedControlPlane{}
	_ executor.ChildTaskControlPlane        = unexpectedControlPlane{}
)
