package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type childTaskControlPlane struct {
	*testRunLeaseControlPlane
	request      workerapi.InvokeChildTaskRequest
	requests     []workerapi.InvokeChildTaskRequest
	response     workerapi.InvokeChildTaskResponse
	err          error
	errors       []error
	firstAttempt chan struct{}
}

func (controlPlane *childTaskControlPlane) InvokeChildTask(
	_ context.Context,
	request workerapi.InvokeChildTaskRequest,
) (workerapi.InvokeChildTaskResponse, error) {
	controlPlane.request = request
	controlPlane.requests = append(controlPlane.requests, request)
	if len(controlPlane.errors) != 0 {
		err := controlPlane.errors[0]
		controlPlane.errors = controlPlane.errors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.InvokeChildTaskResponse{}, err
	}
	return controlPlane.response, controlPlane.err
}

func TestHandleChildTaskInvokeWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc21"
	payload := `{"imageId":"image-1"}`
	idempotencyKey := "resize:image-1"
	controlPlane := &childTaskControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.InvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &workerapi.ChildTaskStartResult{
				RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
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
		decision.GetDataJson() != `{"run_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if controlPlane.request.Lease != lease.Fence() ||
		controlPlane.request.TaskDeclaredID != "resize-image" ||
		controlPlane.request.Method != "start" ||
		!controlPlane.request.PayloadPresent ||
		string(controlPlane.request.Payload) != payload ||
		string(controlPlane.request.Workspace) != `{"key":"image-workspace"}` ||
		string(controlPlane.request.Options) != `{"queue":"priority"}` ||
		controlPlane.request.IdempotencyKey != idempotencyKey {
		t.Fatalf("request = %+v", controlPlane.request)
	}
}

func TestHandleChildTaskInvokeRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc24"
	firstAttempt := make(chan struct{})
	controlPlane := &childTaskControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.InvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &workerapi.ChildTaskStartResult{
				RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3a",
			},
		},
		errors: []error{&httpclient.Error{
			StatusCode: 503,
			Status:     "503 Service Unavailable",
			Message:    "temporary Control Plane failure",
		}},
		firstAttempt: firstAttempt,
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
		lease:        lease,
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
	if len(controlPlane.requests) != 2 {
		t.Fatalf("requests = %+v", controlPlane.requests)
	}
	assertRetriedWithStableFence(t, controlPlane.requests[0].Lease, controlPlane.requests[1].Lease, len(controlPlane.requests))
}

func TestHandleChildTaskCallContinuesOpenedWait(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc23"
	runWaitID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc25"
	resumeAttachID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc26"
	actorSequence := int64(9)
	waitClient := &fakeRunWaitClient{
		polls: []workerapi.RunWaitPollResponse{{
			RunID: lease.RunID, RunWaitID: runWaitID,
			Status:     workerapi.RunWaitPollStatusResumeRequested,
			ResumeKind: "completed",
			ResumePayload: json.RawMessage(
				`{"ok":true,"output":{"resized":true},"run":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}}`,
			),
		}},
	}
	openedWait := liveRunWaitResponse()
	openedWait.RunID = lease.RunID
	openedWait.RunWaitID = runWaitID
	openedWait.ResumeAttachID = resumeAttachID
	controlPlane := &childTaskControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.InvokeChildTaskResponse{
			CorrelationID: correlationID,
			Completed: &workerapi.ChildTaskStartResult{
				RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			},
			OpenedWait: &openedWait,
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
		lease:        lease,
		waits:        &ControlPlaneRunWaits{Client: waitClient},
	}
	result := make(chan error, 1)
	go func() {
		result <- task.handleChildTaskInvoke(t.Context(), &runv0.TaskChildInvokeRequested{
			CorrelationId:                 correlationID,
			RunWaitId:                     runWaitID,
			ResumeAttachId:                resumeAttachID,
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
		decision.GetResumeAttachId() != resumeAttachID ||
		decision.GetKind() != "completed" ||
		decision.GetDataJson() != `{"ok":true,"output":{"resized":true},"run":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}}` {
		t.Fatalf("decision = %+v", decision)
	}
	if controlPlane.request.ActorSpeculativeInputSequence == nil ||
		*controlPlane.request.ActorSpeculativeInputSequence != actorSequence ||
		controlPlane.request.RunWaitID != runWaitID ||
		controlPlane.request.ResumeAttachID != resumeAttachID {
		t.Fatalf("request = %+v", controlPlane.request)
	}
	if waitClient.createdRequest.CorrelationID != "" {
		t.Fatalf("unexpected duplicate Wait registration = %+v", waitClient.createdRequest)
	}
}

func TestHandleChildTaskInvokeReturnsSemanticFailureToRuntime(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc22"
	controlPlane := &childTaskControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		err: &httpclient.Error{
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
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
		lease:        lease,
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
	var failure workerapi.RuntimeOperationFailure
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

func TestChildTaskInvokeFailureDoesNotClassifyGenericConflict(t *testing.T) {
	failure, ok := childTaskInvokeFailure(&httpclient.Error{
		StatusCode: 409,
		Status:     "409 Conflict",
		Code:       "conflict",
		Message:    "child Task invocation authority is stale",
	})
	if ok || failure != (workerapi.RuntimeOperationFailure{}) {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}
