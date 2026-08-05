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
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorRuntimeContractControlPlane struct {
	*testRunLeaseControlPlane
	startRequest  workerapi.StartActorRequest
	startRequests []workerapi.StartActorRequest
	startResponse workerapi.StartActorResponse
	startErr      error
	startErrors   []error
	firstAttempt  chan struct{}
	statusRequest workerapi.SessionReferenceRequest
	closeRequest  workerapi.CloseSessionRequest
	outputRequest workerapi.ReadSessionOutputPageRequest
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
		Completed:     &api.SessionStatusSnapshot{},
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

func (controlPlane *actorRuntimeContractControlPlane) ReadRunSessionOutputPage(
	_ context.Context,
	request workerapi.ReadSessionOutputPageRequest,
) (workerapi.ReadSessionOutputPageResponse, error) {
	controlPlane.outputRequest = request
	return workerapi.ReadSessionOutputPageResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.SessionOutputPage{},
	}, nil
}

func TestWorkerActorStartRequestPreservesInputPresence(t *testing.T) {
	base := func() *runv0.ActorStartRequested {
		return &runv0.ActorStartRequested{
			CorrelationId:  "019c0225-f0c9-7f66-8a23-7782ca0a8461",
			DeclaredId:     "mailbox",
			WorkspaceId:    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
			RunOptionsJson: `{}`,
		}
	}
	omitted, err := workerActorStartRequest(base())
	if err != nil {
		t.Fatal(err)
	}
	if omitted.InputPresent || len(omitted.Input) != 0 {
		t.Fatalf("omitted input = present %v, %q", omitted.InputPresent, omitted.Input)
	}
	null := base()
	value := "null"
	null.InputJson = &value
	present, err := workerActorStartRequest(null)
	if err != nil {
		t.Fatal(err)
	}
	if !present.InputPresent || string(present.Input) != "null" {
		t.Fatalf("null input = present %v, %q", present.InputPresent, present.Input)
	}
}

func TestWorkerSessionReferencesRequireCanonicalCorrelationAndID(t *testing.T) {
	request := &runv0.SessionStatusRequested{
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
	event := &runv0.RunEvent{Event: &runv0.RunEvent_ActorStartRequested{
		ActorStartRequested: &runv0.ActorStartRequested{
			CorrelationId:  correlationID,
			DeclaredId:     "mailbox",
			WorkspaceId:    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
			controlPlane.startRequest.Workspace.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" {
			t.Fatalf("decision = %+v request = %+v", decision, controlPlane.startRequest)
		}
	})
	t.Run("domain failure", func(t *testing.T) {
		controlPlane := &actorRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			startResponse: workerapi.StartActorResponse{
				CorrelationID: correlationID,
				Failed: &workerapi.RuntimeOperationFailure{
					Code: "actor_key_conflict", Message: "Actor key is in use",
				},
			},
		}
		decision, err := runActorRuntimeContract(t, event, controlPlane)
		if err != nil {
			t.Fatal(err)
		}
		var failure workerapi.RuntimeOperationFailure
		if err := json.Unmarshal([]byte(decision.GetDataJson()), &failure); err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "failed" || failure.Code != "actor_key_conflict" {
			t.Fatalf("decision = %+v failure = %+v", decision, failure)
		}
	})
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
		events := []*runv0.RunEvent{
			{Event: &runv0.RunEvent_SessionStatusRequested{
				SessionStatusRequested: &runv0.SessionStatusRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				},
			}},
			{Event: &runv0.RunEvent_SessionCloseRequested{
				SessionCloseRequested: &runv0.SessionCloseRequested{
					CorrelationId: correlationID, SessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
				},
			}},
			{Event: &runv0.RunEvent_SessionOutputPageRequested{
				SessionOutputPageRequested: &runv0.SessionOutputPageRequested{
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
			controlPlane.closeRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.outputRequest.SessionID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
			controlPlane.outputRequest.Limit != 25 {
			t.Fatalf("status=%+v close=%+v output=%+v",
				controlPlane.statusRequest, controlPlane.closeRequest, controlPlane.outputRequest)
		}
	})
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
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
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
		result <- task.handleActorRuntime(t.Context(), &runv0.RunEvent{
			Event: &runv0.RunEvent_ActorStartRequested{
				ActorStartRequested: &runv0.ActorStartRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					WorkspaceId:    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
	t.Run("finalizing task", func(t *testing.T) {
		task := &guestRunLeaseTask{
			lease:          testRunLeaseAssignment(time.Now().Add(time.Minute)),
			finalizingKind: workerapi.RunFinalizationCapture,
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
	event *runv0.RunEvent,
	controlPlane *actorRuntimeContractControlPlane,
) (*runv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
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
