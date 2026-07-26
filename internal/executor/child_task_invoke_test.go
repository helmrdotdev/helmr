package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type childTaskControl struct {
	*testRunLeaseControl
	request      api.WorkerInvokeChildTaskRequest
	requests     []api.WorkerInvokeChildTaskRequest
	response     api.WorkerInvokeChildTaskResponse
	err          error
	errors       []error
	firstAttempt chan struct{}
}

func (control *childTaskControl) InvokeChildTask(
	_ context.Context,
	request api.WorkerInvokeChildTaskRequest,
) (api.WorkerInvokeChildTaskResponse, error) {
	control.request = request
	control.requests = append(control.requests, request)
	if len(control.errors) != 0 {
		err := control.errors[0]
		control.errors = control.errors[1:]
		if control.firstAttempt != nil {
			close(control.firstAttempt)
			control.firstAttempt = nil
		}
		return api.WorkerInvokeChildTaskResponse{}, err
	}
	return control.response, control.err
}

func TestHandleChildTaskInvokeWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000121"
	payload := `{"imageId":"image-1"}`
	idempotencyKey := "resize:image-1"
	control := &childTaskControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerInvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &api.WorkerChildTaskStartResult{
				RunID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
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
	result := make(chan error, 1)
	go func() {
		result <- task.handleChildTaskInvoke(t.Context(), &runv0.TaskChildInvokeRequested{
			CorrelationId:  correlationID,
			DeclaredId:     "resize-image",
			Method:         "start",
			PayloadPresent: true,
			PayloadJson:    &payload,
			WorkspaceJson:  `{"key":"image-workspace"}`,
			OptionsJson:    `{"queue":"priority"}`,
			IdempotencyKey: &idempotencyKey,
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
	if decision.GetCorrelationId() != correlationID ||
		decision.GetKind() != "completed" ||
		decision.GetDataJson() != `{"run_id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.Lease != lease ||
		control.request.TaskDeclaredID != "resize-image" ||
		control.request.Method != "start" ||
		!control.request.PayloadPresent ||
		string(control.request.Payload) != payload ||
		string(control.request.Workspace) != `{"key":"image-workspace"}` ||
		string(control.request.Options) != `{"queue":"priority"}` ||
		control.request.IdempotencyKey != idempotencyKey {
		t.Fatalf("request = %+v", control.request)
	}
}

func TestHandleChildTaskInvokeRetryUsesRenewedReceipt(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000124"
	firstAttempt := make(chan struct{})
	control := &childTaskControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerInvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &api.WorkerChildTaskStartResult{
				RunID: "run_aaaaaaaaaaaaaaaaaaaaaaaaab",
			},
		},
		errors: []error{&client.HTTPError{
			StatusCode: 503,
			Status:     "503 Service Unavailable",
			Message:    "temporary control failure",
		}},
		firstAttempt: firstAttempt,
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
	}
	go renewRunSourceReceiptAfterAttempt(task, firstAttempt)
	result := make(chan error, 1)
	go func() {
		result <- task.handleChildTaskInvoke(t.Context(), &runv0.TaskChildInvokeRequested{
			CorrelationId: correlationID,
			DeclaredId:    "resize-image",
			Method:        "start",
			WorkspaceJson: `{}`,
			OptionsJson:   `{}`,
		})
	}()
	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadResumeDecision(header, reader, bodyLen); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if len(control.requests) != 2 {
		t.Fatalf("requests = %+v", control.requests)
	}
	assertRetriedWithRenewedReceipt(t, control.requests[0].Lease, control.requests[1].Lease, len(control.requests))
}

func TestHandleChildTaskCallContinuesOpenedWait(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000123"
	actorSequence := int64(9)
	waitClient := &fakeRunWaitClient{
		polls: []api.WorkerRunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1",
			Status:     api.WorkerRunWaitPollStatusResumeRequested,
			ResumeKind: "completed",
			ResumePayload: json.RawMessage(
				`{"ok":true,"output":{"resized":true},"run":{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
			),
		}},
	}
	openedWait := liveRunWaitResponse()
	control := &childTaskControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerInvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &api.WorkerChildTaskStartResult{
				RunID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			OpenedWait: &openedWait,
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
		waits:   &ControlRunWaits{Client: waitClient},
	}
	result := make(chan error, 1)
	go func() {
		result <- task.handleChildTaskInvoke(t.Context(), &runv0.TaskChildInvokeRequested{
			CorrelationId:                 correlationID,
			DeclaredId:                    "resize-image",
			Method:                        "call",
			WorkspaceJson:                 `{"key":"image-workspace"}`,
			OptionsJson:                   `{}`,
			ActorSpeculativeInputSequence: &actorSequence,
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
	if decision.GetRunWaitId() != openedWait.RunWaitID ||
		decision.GetCorrelationId() != correlationID ||
		decision.GetKind() != "completed" ||
		decision.GetDataJson() != `{"ok":true,"output":{"resized":true},"run":{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}` {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.ActorSpeculativeInputSequence == nil ||
		*control.request.ActorSpeculativeInputSequence != actorSequence {
		t.Fatalf("request = %+v", control.request)
	}
	if waitClient.createdRequest.CorrelationID != "" {
		t.Fatalf("unexpected duplicate Wait registration = %+v", waitClient.createdRequest)
	}
}

func TestHandleChildTaskInvokeReturnsSemanticFailureToRuntime(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000122"
	control := &childTaskControl{
		testRunLeaseControl: &testRunLeaseControl{},
		err: &client.HTTPError{
			StatusCode: 409,
			Status:     "409 Conflict",
			Message:    "idempotency key conflicts with an earlier child Task invocation",
			Code:       "idempotency_conflict",
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
	result := make(chan error, 1)
	go func() {
		result <- task.handleChildTaskInvoke(t.Context(), &runv0.TaskChildInvokeRequested{
			CorrelationId: correlationID,
			DeclaredId:    "resize-image",
			Method:        "start",
			WorkspaceJson: `{"key":"image-workspace"}`,
			OptionsJson:   `{}`,
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
	var failure api.WorkerRuntimeOperationFailure
	if err := json.Unmarshal([]byte(decision.GetDataJson()), &failure); err != nil {
		t.Fatal(err)
	}
	if decision.GetCorrelationId() != correlationID ||
		decision.GetKind() != "failed" ||
		failure.Code != "idempotency_conflict" ||
		failure.Retryable {
		t.Fatalf("decision = %+v failure = %+v", decision, failure)
	}
}

func TestChildTaskInvokeFailureDoesNotClassifyUncodedConflict(t *testing.T) {
	failure, ok := childTaskInvokeFailure(&client.HTTPError{
		StatusCode: 409,
		Status:     "409 Conflict",
		Message:    "child Task invocation authority is stale",
	})
	if ok || failure != (api.WorkerRuntimeOperationFailure{}) {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}
