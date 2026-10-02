package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorRuntimeContractControlPlane struct {
	*testRunLeaseControlPlane
	startRequest   workerapi.StartActorRequest
	startRequests  []workerapi.StartActorRequest
	startResponse  workerapi.StartActorResponse
	startErr       error
	startErrors    []error
	firstAttempt   chan struct{}
	statusRequest  workerapi.SessionReferenceRequest
	closeRequest   workerapi.CloseSessionRequest
	cancelRequest  workerapi.CancelSessionRequest
	cancelRequests []workerapi.CancelSessionRequest
	cancelErrors   []error
	outputRequest  workerapi.ReadSessionEventsRequest
}

func (controlPlane *actorRuntimeContractControlPlane) StartRunActor(
	_ context.Context,
	request workerapi.StartActorRequest,
) (workerapi.StartActorResponse, error) {
	controlPlane.startRequest = request
	controlPlane.startRequests = append(controlPlane.startRequests, request)
	if len(controlPlane.startErrors) != 0 {
		err := controlPlane.startErrors[0]
		controlPlane.startErrors = controlPlane.startErrors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.StartActorResponse{}, err
	}
	return controlPlane.startResponse, controlPlane.startErr
}

func (controlPlane *actorRuntimeContractControlPlane) GetRunSessionStatus(
	_ context.Context,
	request workerapi.SessionReferenceRequest,
) (workerapi.SessionStatusResponse, error) {
	controlPlane.statusRequest = request
	return workerapi.SessionStatusResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.Session{},
	}, nil
}

func (controlPlane *actorRuntimeContractControlPlane) CloseRunSession(
	_ context.Context,
	request workerapi.CloseSessionRequest,
) (workerapi.CloseSessionResponse, error) {
	controlPlane.closeRequest = request
	return workerapi.CloseSessionResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.SessionCloseReceipt{},
	}, nil
}

func (controlPlane *actorRuntimeContractControlPlane) CancelRunSession(
	_ context.Context,
	request workerapi.CancelSessionRequest,
) (workerapi.CancelSessionResponse, error) {
	controlPlane.cancelRequest = request
	controlPlane.cancelRequests = append(controlPlane.cancelRequests, request)
	if len(controlPlane.cancelErrors) != 0 {
		err := controlPlane.cancelErrors[0]
		controlPlane.cancelErrors = controlPlane.cancelErrors[1:]
		return workerapi.CancelSessionResponse{}, err
	}
	return workerapi.CancelSessionResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.SessionCancelReceipt{},
	}, nil
}

func (controlPlane *actorRuntimeContractControlPlane) ReadRunSessionEvents(
	_ context.Context,
	request workerapi.ReadSessionEventsRequest,
) (workerapi.ReadSessionEventsResponse, error) {
	controlPlane.outputRequest = request
	return workerapi.ReadSessionEventsResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.SessionEventPage{},
	}, nil
}

func TestWorkerSessionReferencesRequireCanonicalCorrelationAndID(t *testing.T) {
	request := &programv0.SessionStatusRequested{
		CorrelationId: "019c0225-f0c9-7f66-8a23-7782ca0a8461",
		SessionId:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
	}
	if _, err := workerSessionReferenceRequest(request); err != nil {
		t.Fatal(err)
	}
	request.CorrelationId = "019C0225-F0C9-7F66-8A23-7782CA0A8461"
	if _, err := workerSessionReferenceRequest(request); err == nil {
		t.Fatal("non-canonical correlation ID was accepted")
	}
	request.CorrelationId = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	request.SessionId = ""
	if _, err := workerSessionReferenceRequest(request); err == nil {
		t.Fatal("missing Session ID was accepted")
	}
}

func TestActorRuntimeVerticalContract(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	event := &programv0.RunEvent{Event: &programv0.RunEvent_ActorStartRequested{
		ActorStartRequested: &programv0.ActorStartRequested{
			CorrelationId:  correlationID,
			DeclaredId:     "mailbox",
			ComputerId:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
			RunOptionsJson: `{}`,
		},
	}}
	t.Run("happy path", func(t *testing.T) {
		controlPlane := &actorRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			startResponse: workerapi.StartActorResponse{
				CorrelationID: correlationID,
				Completed: &api.StartActorResponse{
					SessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
					RunID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
				},
			},
		}
		decision, err := runActorRuntimeContract(t, event, controlPlane)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			controlPlane.startRequest.Lease.ID == "" ||
			controlPlane.startRequest.ActorDeclaredID != "mailbox" ||
			controlPlane.startRequest.Computer.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" {
			t.Fatalf("decision = %+v request = %+v", decision, controlPlane.startRequest)
		}
	})
	for _, code := range []string{"actor_key_conflict", "computer_preparation_exhausted", "invalid_actor_start"} {
		t.Run(code, func(t *testing.T) {
			controlPlane := &actorRuntimeContractControlPlane{
				testRunLeaseControlPlane: &testRunLeaseControlPlane{},
				startResponse:            workerapi.StartActorResponse{CorrelationID: correlationID, Failed: &workerapi.RuntimeOperationFailure{Code: code, Message: "Actor start rejected"}},
			}
			decision, err := runActorRuntimeContract(t, event, controlPlane)
			if err != nil {
				t.Fatal(err)
			}
			var failure workerapi.RuntimeOperationFailure
			if err = json.Unmarshal([]byte(decision.GetDataJson()), &failure); err != nil {
				t.Fatal(err)
			}
			if decision.GetKind() != "failed" || failure.Code != code || failure.Retryable || len(controlPlane.startRequests) != 1 {
				t.Fatalf("decision=%+v failure=%+v requests=%d", decision, failure, len(controlPlane.startRequests))
			}
		})
	}
	t.Run("stale source fence", func(t *testing.T) {
		controlPlane := &actorRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			startErr: &httpclient.Error{
				StatusCode: 409, Status: "409 Conflict",
				Message: "worker Run source authority is stale",
			},
		}
		_, err := runActorRuntimeContract(t, event, controlPlane)
		if err == nil {
			t.Fatal("stale source was accepted")
		}
	})
	t.Run("status close and output branches", func(t *testing.T) {
		controlPlane := &actorRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		}
		events := []*programv0.RunEvent{
			{Event: &programv0.RunEvent_SessionStatusRequested{
				SessionStatusRequested: &programv0.SessionStatusRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				},
			}},
			{Event: &programv0.RunEvent_SessionCloseRequested{
				SessionCloseRequested: &programv0.SessionCloseRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				},
			}},
			{Event: &programv0.RunEvent_SessionCancelRequested{
				SessionCancelRequested: &programv0.SessionCancelRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				},
			}},
			{Event: &programv0.RunEvent_SessionEventsRequested{
				SessionEventsRequested: &programv0.SessionEventsRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
					Limit: 25,
				},
			}},
		}
		for _, branch := range events {
			decision, err := runActorRuntimeContract(t, branch, controlPlane)
			if err != nil {
				t.Fatal(err)
			}
			if decision.GetKind() != "completed" {
				t.Fatalf("decision = %+v", decision)
			}
		}
		if controlPlane.statusRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.cancelRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.closeRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.outputRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.outputRequest.Limit != 25 {
			t.Fatalf("status=%+v close=%+v output=%+v",
				controlPlane.statusRequest, controlPlane.closeRequest, controlPlane.outputRequest)
		}
	})
}

func TestSessionCancelRetriesTargetChangeWithSameRequest(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	controlPlane := &actorRuntimeContractControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		cancelErrors: []error{&httpclient.Error{
			StatusCode: 503, Status: "503 Service Unavailable",
			Message: "Session control target changed; retry the operation",
		}},
	}
	decision, err := runActorRuntimeContract(t, &programv0.RunEvent{
		Event: &programv0.RunEvent_SessionCancelRequested{
			SessionCancelRequested: &programv0.SessionCancelRequested{
				CorrelationId:  correlationID,
				SessionId:      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				IdempotencyKey: new("cancel-1"),
			},
		},
	}, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if decision.GetKind() != "completed" || decision.GetCorrelationId() != correlationID ||
		len(controlPlane.cancelRequests) != 2 ||
		controlPlane.cancelRequests[0] != controlPlane.cancelRequests[1] ||
		controlPlane.cancelRequest.IdempotencyKey != "cancel-1" {
		t.Fatalf("decision=%+v requests=%+v", decision, controlPlane.cancelRequests)
	}
}

func TestActorRuntimeRetryUsesRenewedAssignment(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	firstAttempt := make(chan struct{})
	controlPlane := &actorRuntimeContractControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		firstAttempt:             firstAttempt,
		startErrors: []error{&httpclient.Error{
			StatusCode: 503, Status: "503 Service Unavailable",
			Message: "temporary Control Plane failure",
		}},
		startResponse: workerapi.StartActorResponse{
			CorrelationID: correlationID,
			Completed: &api.StartActorResponse{
				SessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				RunID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			},
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{channel: fakeGuestMachine{stream: guest}},
		controlPlane: testControlPlane(t, controlPlane),
		lease:        lease,
	}
	go func() {
		<-firstAttempt
		task.mu.Lock()
		task.lease.ExpiresAt = task.lease.ExpiresAt.Add(time.Minute)
		task.mu.Unlock()
	}()
	result := make(chan error, 1)
	go func() {
		result <- task.handleActorRuntime(t.Context(), &programv0.RunEvent{
			Event: &programv0.RunEvent_ActorStartRequested{
				ActorStartRequested: &programv0.ActorStartRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					ComputerId:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
					RunOptionsJson: `{}`,
				},
			},
		})
	}()
	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if decision.GetKind() != "completed" ||
		len(controlPlane.startRequests) != 2 ||
		controlPlane.startRequests[1].Lease != controlPlane.startRequests[0].Lease {
		t.Fatalf("decision=%+v requests=%+v", decision, controlPlane.startRequests)
	}
}

func TestRunSourceRuntimeRejectsTerminalLocalStateWithoutRetry(t *testing.T) {
	t.Run("expired receipt", func(t *testing.T) {
		task := &guestRunLeaseTask{lease: testRunLeaseAssignment(time.Now().Add(-time.Second))}
		calls := 0
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := task.callRunSourceRuntime(ctx, func(
			context.Context,
			workerapi.RunLeaseAssignment,
		) error {
			calls++
			return nil
		})
		if !errors.Is(err, errRunLeaseAuthorityLapsed) || calls != 0 {
			t.Fatalf("error = %v calls = %d", err, calls)
		}
	})
	t.Run("closed task", func(t *testing.T) {
		task := &guestRunLeaseTask{
			lease:    testRunLeaseAssignment(time.Now().Add(time.Minute)),
			finished: true,
		}
		calls := 0
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := task.callRunSourceRuntime(ctx, func(
			context.Context,
			workerapi.RunLeaseAssignment,
		) error {
			calls++
			return nil
		})
		if !errors.Is(err, errRunSourceOperationUnavailable) || calls != 0 {
			t.Fatalf("error = %v calls = %d", err, calls)
		}
	})
}

func TestRunSourceRuntimeCapsAttemptBeforeAssignmentExpiry(t *testing.T) {
	task := &guestRunLeaseTask{
		lease: testRunLeaseAssignment(time.Now().Add(800 * time.Millisecond)),
	}
	firstAttempt := make(chan struct{})
	renewed := make(chan struct{})
	var leases []workerapi.RunLeaseAssignment
	go func() {
		<-firstAttempt
		task.mu.Lock()
		task.lease.ExpiresAt = time.Now().Add(time.Minute)
		task.mu.Unlock()
		close(renewed)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		leases = append(leases, lease)
		if len(leases) == 1 {
			close(firstAttempt)
			<-callCtx.Done()
			return callCtx.Err()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-renewed:
	default:
		t.Fatal("renewal did not acquire the task lock between attempts")
	}
	if len(leases) != 2 {
		t.Fatalf("leases = %+v", leases)
	}
	if leases[1].Fence() != leases[0].Fence() ||
		!leases[1].ExpiresAt.After(leases[0].ExpiresAt) {
		t.Fatalf("first lease = %+v second lease = %+v", leases[0], leases[1])
	}
}

func runActorRuntimeContract(
	t *testing.T,
	event *programv0.RunEvent,
	controlPlane *actorRuntimeContractControlPlane,
) (*programv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{channel: fakeGuestMachine{stream: guest}},
		controlPlane: testControlPlane(t, controlPlane),
		lease:        lease,
	}
	result := make(chan error, 1)
	go func() { result <- task.handleActorRuntime(t.Context(), event) }()
	if controlPlane.startErr != nil {
		return nil, <-result
	}
	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		return nil, err
	}
	decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
	if err != nil {
		return nil, err
	}
	return decision, <-result
}
