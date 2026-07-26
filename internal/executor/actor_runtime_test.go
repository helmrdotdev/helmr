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
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type actorRuntimeContractControl struct {
	*testRunLeaseControl
	startRequest  api.WorkerStartActorRequest
	startRequests []api.WorkerStartActorRequest
	startResponse api.WorkerStartActorResponse
	startErr      error
	startErrors   []error
	firstAttempt  chan struct{}
	statusRequest api.WorkerActorReferenceRequest
	closeRequest  api.WorkerCloseActorRequest
	outputRequest api.WorkerReadActorOutputPageRequest
}

func (control *actorRuntimeContractControl) StartRunActor(
	_ context.Context,
	request api.WorkerStartActorRequest,
) (api.WorkerStartActorResponse, error) {
	control.startRequest = request
	control.startRequests = append(control.startRequests, request)
	if len(control.startErrors) != 0 {
		err := control.startErrors[0]
		control.startErrors = control.startErrors[1:]
		if control.firstAttempt != nil {
			close(control.firstAttempt)
			control.firstAttempt = nil
		}
		return api.WorkerStartActorResponse{}, err
	}
	return control.startResponse, control.startErr
}

func (control *actorRuntimeContractControl) GetRunActorStatus(
	_ context.Context,
	request api.WorkerActorReferenceRequest,
) (api.WorkerActorStatusResponse, error) {
	control.statusRequest = request
	return api.WorkerActorStatusResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.ActorStatus{},
	}, nil
}

func (control *actorRuntimeContractControl) CloseRunActor(
	_ context.Context,
	request api.WorkerCloseActorRequest,
) (api.WorkerCloseActorResponse, error) {
	control.closeRequest = request
	return api.WorkerCloseActorResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.ActorOperationReceipt{},
	}, nil
}

func (control *actorRuntimeContractControl) ReadRunActorOutputPage(
	_ context.Context,
	request api.WorkerReadActorOutputPageRequest,
) (api.WorkerReadActorOutputPageResponse, error) {
	control.outputRequest = request
	return api.WorkerReadActorOutputPageResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.ActorOutputPage{},
	}, nil
}

func TestWorkerActorStartRequestPreservesInputPresence(t *testing.T) {
	base := func() *runv0.ActorStartRequested {
		return &runv0.ActorStartRequested{
			CorrelationId: "019c0225-f0c9-7f66-8a23-7782ca0a8461",
			DeclaredId:    "mailbox",
			Workspace: &runv0.ActorStartRequested_WorkspaceKey{
				WorkspaceKey: "actor-workspace",
			},
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

func TestWorkerActorReferencesRequireCanonicalCorrelationAndExactAddress(t *testing.T) {
	request := &runv0.ActorStatusRequested{
		CorrelationId: "019c0225-f0c9-7f66-8a23-7782ca0a8461",
		DeclaredId:    "mailbox",
		Address: &runv0.ActorStatusRequested_ActorKey{
			ActorKey: "primary",
		},
	}
	if _, err := workerActorReferenceRequest(request); err != nil {
		t.Fatal(err)
	}
	request.CorrelationId = "019C0225-F0C9-7F66-8A23-7782CA0A8461"
	if _, err := workerActorReferenceRequest(request); err == nil {
		t.Fatal("non-canonical correlation ID was accepted")
	}
	request.CorrelationId = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	request.Address = nil
	if _, err := workerActorReferenceRequest(request); err == nil {
		t.Fatal("missing Actor address was accepted")
	}
}

func TestActorRuntimeVerticalContract(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	event := &runv0.RunEvent{Event: &runv0.RunEvent_ActorStartRequested{
		ActorStartRequested: &runv0.ActorStartRequested{
			CorrelationId: correlationID,
			DeclaredId:    "mailbox",
			Workspace: &runv0.ActorStartRequested_WorkspaceKey{
				WorkspaceKey: "mailbox-data",
			},
			RunOptionsJson: `{}`,
		},
	}}
	t.Run("happy path", func(t *testing.T) {
		control := &actorRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
			startResponse: api.WorkerStartActorResponse{
				CorrelationID: correlationID,
				Completed: &api.StartActorResponse{
					ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
					RunID:   "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			},
		}
		decision, err := runActorRuntimeContract(t, event, control)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			control.startRequest.Lease.ID == "" ||
			control.startRequest.ActorDeclaredID != "mailbox" ||
			control.startRequest.Workspace.Key == nil ||
			*control.startRequest.Workspace.Key != "mailbox-data" {
			t.Fatalf("decision = %+v request = %+v", decision, control.startRequest)
		}
	})
	t.Run("domain failure", func(t *testing.T) {
		control := &actorRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
			startResponse: api.WorkerStartActorResponse{
				CorrelationID: correlationID,
				Failed: &api.WorkerRuntimeOperationFailure{
					Code: "actor_key_conflict", Message: "Actor key is in use",
				},
			},
		}
		decision, err := runActorRuntimeContract(t, event, control)
		if err != nil {
			t.Fatal(err)
		}
		var failure api.WorkerRuntimeOperationFailure
		if err := json.Unmarshal([]byte(decision.GetDataJson()), &failure); err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "failed" || failure.Code != "actor_key_conflict" {
			t.Fatalf("decision = %+v failure = %+v", decision, failure)
		}
	})
	t.Run("stale source fence", func(t *testing.T) {
		control := &actorRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
			startErr: &client.HTTPError{
				StatusCode: 409, Status: "409 Conflict",
				Message: "worker Run source authority is stale",
			},
		}
		_, err := runActorRuntimeContract(t, event, control)
		if err == nil {
			t.Fatal("stale source was accepted")
		}
	})
	t.Run("status close and output branches", func(t *testing.T) {
		control := &actorRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
		}
		events := []*runv0.RunEvent{
			{Event: &runv0.RunEvent_ActorStatusRequested{
				ActorStatusRequested: &runv0.ActorStatusRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					Address: &runv0.ActorStatusRequested_ActorKey{ActorKey: "primary"},
				},
			}},
			{Event: &runv0.RunEvent_ActorCloseRequested{
				ActorCloseRequested: &runv0.ActorCloseRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					Address: &runv0.ActorCloseRequested_ActorKey{ActorKey: "primary"},
				},
			}},
			{Event: &runv0.RunEvent_ActorOutputPageRequested{
				ActorOutputPageRequested: &runv0.ActorOutputPageRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					Address: &runv0.ActorOutputPageRequested_ActorKey{ActorKey: "primary"},
					Limit:   25,
				},
			}},
		}
		for _, branch := range events {
			decision, err := runActorRuntimeContract(t, branch, control)
			if err != nil {
				t.Fatal(err)
			}
			if decision.GetKind() != "completed" {
				t.Fatalf("decision = %+v", decision)
			}
		}
		if control.statusRequest.ActorKey != "primary" ||
			control.closeRequest.ActorKey != "primary" ||
			control.outputRequest.ActorKey != "primary" ||
			control.outputRequest.Limit != 25 {
			t.Fatalf("status=%+v close=%+v output=%+v",
				control.statusRequest, control.closeRequest, control.outputRequest)
		}
	})
}

func TestActorRuntimeRetryUsesRenewedReceipt(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	firstAttempt := make(chan struct{})
	control := &actorRuntimeContractControl{
		testRunLeaseControl: &testRunLeaseControl{},
		firstAttempt:        firstAttempt,
		startErrors: []error{&client.HTTPError{
			StatusCode: 503, Status: "503 Service Unavailable",
			Message: "temporary control failure",
		}},
		startResponse: api.WorkerStartActorResponse{
			CorrelationID: correlationID,
			Completed: &api.StartActorResponse{
				ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
				RunID:   "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
	}
	go func() {
		<-firstAttempt
		task.mu.Lock()
		task.lease.LeaseSequence++
		task.lease.ExpiresAt = task.lease.ExpiresAt.Add(time.Minute)
		task.mu.Unlock()
	}()
	result := make(chan error, 1)
	go func() {
		result <- task.handleActorRuntime(t.Context(), &runv0.RunEvent{
			Event: &runv0.RunEvent_ActorStartRequested{
				ActorStartRequested: &runv0.ActorStartRequested{
					CorrelationId: correlationID, DeclaredId: "mailbox",
					Workspace: &runv0.ActorStartRequested_WorkspaceKey{
						WorkspaceKey: "mailbox-data",
					},
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
		len(control.startRequests) != 2 ||
		control.startRequests[1].Lease.LeaseSequence !=
			control.startRequests[0].Lease.LeaseSequence+1 ||
		!control.startRequests[1].Lease.ExpiresAt.After(
			control.startRequests[0].Lease.ExpiresAt,
		) {
		t.Fatalf("decision=%+v requests=%+v", decision, control.startRequests)
	}
}

func TestRunSourceRuntimeRejectsTerminalLocalStateWithoutRetry(t *testing.T) {
	t.Run("expired receipt", func(t *testing.T) {
		task := &guestRunLeaseTask{lease: testRunLeaseReceipt(time.Now().Add(-time.Second))}
		calls := 0
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := task.callRunSourceRuntime(ctx, func(
			context.Context,
			api.WorkerRunLeaseReceipt,
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
			lease:          testRunLeaseReceipt(time.Now().Add(time.Minute)),
			finalizingKind: api.WorkerRunFinalizationCapture,
		}
		calls := 0
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := task.callRunSourceRuntime(ctx, func(
			context.Context,
			api.WorkerRunLeaseReceipt,
		) error {
			calls++
			return nil
		})
		if !errors.Is(err, errRunSourceOperationUnavailable) || calls != 0 {
			t.Fatalf("error = %v calls = %d", err, calls)
		}
	})
}

func TestRunSourceRuntimeCapsAttemptBeforeReceiptExpiry(t *testing.T) {
	task := &guestRunLeaseTask{
		lease: testRunLeaseReceipt(time.Now().Add(800 * time.Millisecond)),
	}
	firstAttempt := make(chan struct{})
	renewed := make(chan struct{})
	var leases []api.WorkerRunLeaseReceipt
	go func() {
		<-firstAttempt
		task.mu.Lock()
		task.lease.LeaseSequence++
		task.lease.ExpiresAt = time.Now().Add(time.Minute)
		task.mu.Unlock()
		close(renewed)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseReceipt,
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
	assertRetriedWithRenewedReceipt(t, leases[0], leases[1], len(leases))
}

func runActorRuntimeContract(
	t *testing.T,
	event *runv0.RunEvent,
	control *actorRuntimeContractControl,
) (*runv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
	}
	result := make(chan error, 1)
	go func() { result <- task.handleActorRuntime(t.Context(), event) }()
	if control.startErr != nil {
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
