package executor

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorInputSendControlPlane struct {
	*testRunLeaseControlPlane
	request      workerapi.SubmitSessionDataRequest
	requests     []workerapi.SubmitSessionDataRequest
	response     workerapi.SubmitSessionDataResponse
	errors       []error
	firstAttempt chan struct{}
}

func (controlPlane *actorInputSendControlPlane) SendRunSession(
	_ context.Context,
	request workerapi.SubmitSessionDataRequest,
) (workerapi.SubmitSessionDataResponse, error) {
	controlPlane.request = request
	controlPlane.requests = append(controlPlane.requests, request)
	if len(controlPlane.errors) != 0 {
		err := controlPlane.errors[0]
		controlPlane.errors = controlPlane.errors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.SubmitSessionDataResponse{}, err
	}
	return controlPlane.response, nil
}

func (c *actorInputSendControlPlane) EnqueueRunSession(ctx context.Context, r workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	return c.SendRunSession(ctx, r)
}
func (c *actorInputSendControlPlane) SendRunTurnMessage(ctx context.Context, r workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	return c.SendRunSession(ctx, r)
}

func TestHandleSessionSubmitWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000111"
	controlPlane := &actorInputSendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.SubmitSessionDataResponse{
			CorrelationID: correlationID,
			Completed:     &api.SessionAdmissionReceipt{ID: "019c10d5-a6f7-7af1-8f5f-000000000112", TurnID: "019c10d5-a6f7-7af1-8f5f-000000000112", Kind: "turn"},
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
		result <- task.handleSessionSubmit(t.Context(), &programv0.SessionSubmitRequested{Mode: "send",
			CorrelationId:  correlationID,
			SessionId:      "019c10d5-a6f7-7af1-8f5f-000000000111",
			DataJson:       `{"hello":"world"}`,
			IdempotencyKey: new("send-1"),
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
		decision.GetDataJson() != `{"id":"019c10d5-a6f7-7af1-8f5f-000000000112","kind":"turn","turn_id":"019c10d5-a6f7-7af1-8f5f-000000000112"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if controlPlane.request.Lease != lease.Fence() ||
		controlPlane.request.SessionID != "019c10d5-a6f7-7af1-8f5f-000000000111" ||
		controlPlane.request.IdempotencyKey != "send-1" {
		t.Fatalf("request = %+v", controlPlane.request)
	}
}

func TestHandleSessionSubmitRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000113"
	firstAttempt := make(chan struct{})
	controlPlane := &actorInputSendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.SubmitSessionDataResponse{
			CorrelationID: correlationID,
			Completed:     &api.SessionAdmissionReceipt{ID: "019c10d5-a6f7-7af1-8f5f-000000000112", TurnID: "019c10d5-a6f7-7af1-8f5f-000000000112", Kind: "turn"},
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
		result <- task.handleSessionSubmit(t.Context(), &programv0.SessionSubmitRequested{Mode: "send",
			CorrelationId: correlationID,
			SessionId:     "019c10d5-a6f7-7af1-8f5f-000000000111",
			DataJson:      `{"hello":"again"}`,
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

func renewRunSourceReceiptAfterAttempt(task *guestRunLeaseTask, attempted <-chan struct{}) {
	<-attempted
	task.mu.Lock()
	task.lease.ExpiresAt = task.lease.ExpiresAt.Add(time.Minute)
	task.mu.Unlock()
}

func assertRetriedWithStableFence(
	t *testing.T,
	first workerapi.RunLeaseFence,
	second workerapi.RunLeaseFence,
	count int,
) {
	t.Helper()
	if count != 2 || second != first {
		t.Fatalf("request count = %d first lease = %+v second lease = %+v", count, first, second)
	}
}
