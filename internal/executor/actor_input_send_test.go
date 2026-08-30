package executor

import (
	"bufio"
	"context"
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

type actorInputSendControlPlane struct {
	*testRunLeaseControlPlane
	request      workerapi.SendActorInputRequest
	requests     []workerapi.SendActorInputRequest
	response     workerapi.SendActorInputResponse
	errors       []error
	firstAttempt chan struct{}
}

func (controlPlane *actorInputSendControlPlane) SendRunActorInput(
	_ context.Context,
	request workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	controlPlane.request = request
	controlPlane.requests = append(controlPlane.requests, request)
	if len(controlPlane.errors) != 0 {
		err := controlPlane.errors[0]
		controlPlane.errors = controlPlane.errors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.SendActorInputResponse{}, err
	}
	return controlPlane.response, nil
}

func (controlPlane *actorInputSendControlPlane) AppendActorOutput(
	context.Context,
	workerapi.AppendActorOutputRequest,
) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected actor output append")
}

func TestHandleActorInputSendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000111"
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	controlPlane := &actorInputSendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.SendActorInputResponse{
			CorrelationID: correlationID,
			Completed: &api.SessionInput{
				ID: "019c10d5-a6f7-7af1-8f5f-000000000112", Sequence: 7,
				Data: []byte(`{"hello":"world"}`), Source: api.SessionInputSource{Type: "external"},
				CreatedAt: createdAt,
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
		result <- task.handleActorInputSend(t.Context(), &programv0.SessionInputSendRequested{
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
		decision.GetDataJson() != `{"id":"019c10d5-a6f7-7af1-8f5f-000000000112","sequence":7,"data":{"hello":"world"},"source":{"type":"external"},"created_at":"2030-01-02T03:04:05Z"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if controlPlane.request.Lease != lease.Fence() ||
		controlPlane.request.SessionID != "019c10d5-a6f7-7af1-8f5f-000000000111" ||
		controlPlane.request.IdempotencyKey != "send-1" {
		t.Fatalf("request = %+v", controlPlane.request)
	}
}

func TestHandleActorInputSendRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000113"
	firstAttempt := make(chan struct{})
	controlPlane := &actorInputSendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.SendActorInputResponse{
			CorrelationID: correlationID,
			Completed:     &api.SessionInput{Sequence: 8},
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
		result <- task.handleActorInputSend(t.Context(), &programv0.SessionInputSendRequested{
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
