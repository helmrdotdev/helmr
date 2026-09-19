package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorOutputAppendControlPlane struct {
	SessionExecutionControlPlane
	*testRunLeaseControlPlane
	request      workerapi.WriteTurnOutputRequest
	requests     []workerapi.WriteTurnOutputRequest
	response     workerapi.WriteOutputResponse
	errors       []error
	firstAttempt chan struct{}
}

func (controlPlane *actorOutputAppendControlPlane) WriteTurnOutput(
	_ context.Context,
	request workerapi.WriteTurnOutputRequest,
) (workerapi.WriteOutputResponse, error) {
	controlPlane.request = request
	controlPlane.requests = append(controlPlane.requests, request)
	if len(controlPlane.errors) != 0 {
		err := controlPlane.errors[0]
		controlPlane.errors = controlPlane.errors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.WriteOutputResponse{}, err
	}
	return controlPlane.response, nil
}

func TestHandleTurnOutputWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	execution := testTurnExecution(lease)
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000112"
	controlPlane := &actorOutputAppendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.WriteOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.SessionEvent{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34", Sequence: 8,
				Data: json.RawMessage(`{"status":"working"}`), Kind: "output", SessionID: execution.Session.SessionId, TurnID: &execution.TurnId, Provenance: &api.SessionEventProvenance{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunGeneration: execution.Session.RunGeneration},
			},
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}, execution: execution.Session},
		controlPlane: controlPlane,
		lease:        lease,
	}
	result := make(chan error, 1)
	go func() {
		result <- task.handleTurnOutput(t.Context(), &programv0.TurnOutputWriteRequested{Execution: execution,
			CorrelationId:  correlationID,
			DataJson:       `{"status":"working"}`,
			IdempotencyKey: new("output-1"),
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
	if decision.GetCorrelationId() != correlationID || decision.GetKind() != "completed" {
		t.Fatalf("decision = %+v", decision)
	}
	if controlPlane.request.Lease != lease.Fence() ||
		string(controlPlane.request.Data) != `{"status":"working"}` ||
		controlPlane.request.IdempotencyKey != "output-1" {
		t.Fatalf("request = %+v", controlPlane.request)
	}
}

func TestHandleTurnOutputRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	execution := testTurnExecution(lease)
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000114"
	firstAttempt := make(chan struct{})
	controlPlane := &actorOutputAppendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.WriteOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.SessionEvent{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Sequence: 9,
				Data: json.RawMessage(`{"status":"done"}`), Kind: "output", SessionID: execution.Session.SessionId, TurnID: &execution.TurnId, Provenance: &api.SessionEventProvenance{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunGeneration: execution.Session.RunGeneration},
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
		program:      freshProgram{session: fakeGuestSession{stream: guest}, execution: execution.Session},
		controlPlane: controlPlane,
		lease:        lease,
	}
	go renewRunSourceReceiptAfterAttempt(task, firstAttempt)
	result := make(chan error, 1)
	go func() {
		result <- task.handleTurnOutput(t.Context(), &programv0.TurnOutputWriteRequested{Execution: execution,
			CorrelationId: correlationID,
			DataJson:      `{"status":"done"}`,
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

func testTurnExecution(lease workerapi.RunLeaseAssignment) *programv0.TurnExecution {
	return &programv0.TurnExecution{Session: &programv0.SessionExecution{SessionId: "019c10d5-a6f7-7af1-8f5f-000000000111", RunId: lease.RunID, AttemptNumber: uint32(lease.AttemptNumber), RunGeneration: 3}, TurnId: "019c10d5-a6f7-7af1-8f5f-000000000112"}
}

func TestTurnOutputRejectsReceiptFromSuccessorGeneration(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	execution := testTurnExecution(lease)
	cp := &actorOutputAppendControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, response: workerapi.WriteOutputResponse{
		CorrelationID: execution.TurnId, Completed: &api.SessionEvent{ID: "output", SessionID: execution.Session.SessionId, TurnID: &execution.TurnId, Sequence: 1, Provenance: &api.SessionEventProvenance{RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunGeneration: execution.Session.RunGeneration + 1}},
	}}
	task := &guestRunLeaseTask{program: freshProgram{execution: execution.Session}, lease: lease, controlPlane: cp}
	if err := task.handleTurnOutput(t.Context(), &programv0.TurnOutputWriteRequested{CorrelationId: execution.TurnId, Execution: execution, DataJson: "null", MessageDeliveryId: new("019c10d5-a6f7-7af1-8f5f-000000000119")}); err == nil {
		t.Fatal("successor receipt was exposed")
	}
	if len(cp.requests) != 1 || cp.request.MessageDeliveryID == nil || *cp.request.MessageDeliveryID != "019c10d5-a6f7-7af1-8f5f-000000000119" {
		t.Fatalf("callback identity lost: %+v", cp.request)
	}
}
