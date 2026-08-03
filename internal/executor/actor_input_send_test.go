package executor

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorInputSendControl struct {
	*testRunLeaseControl
	request      workerapi.SendActorInputRequest
	requests     []workerapi.SendActorInputRequest
	response     workerapi.SendActorInputResponse
	errors       []error
	firstAttempt chan struct{}
}

func (control *actorInputSendControl) SendRunActorInput(
	_ context.Context,
	request workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	control.request = request
	control.requests = append(control.requests, request)
	if len(control.errors) != 0 {
		err := control.errors[0]
		control.errors = control.errors[1:]
		if control.firstAttempt != nil {
			close(control.firstAttempt)
			control.firstAttempt = nil
		}
		return workerapi.SendActorInputResponse{}, err
	}
	return control.response, nil
}

func (control *actorInputSendControl) AppendActorOutput(
	context.Context,
	workerapi.AppendActorOutputRequest,
) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected Actor output append")
}

func TestHandleActorInputSendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000111"
	control := &actorInputSendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: workerapi.SendActorInputResponse{
			CorrelationID: correlationID,
			Completed:     &api.SendActorInputResponse{Sequence: 7},
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
		result <- task.handleActorInputSend(t.Context(), &runv0.ActorInputSendRequested{
			CorrelationId: correlationID,
			DeclaredId:    "mailbox",
			Address: &runv0.ActorInputSendRequested_ActorKey{
				ActorKey: "primary",
			},
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
		decision.GetDataJson() != `{"sequence":7}` {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.Lease != lease.Fence() ||
		control.request.ActorDeclaredID != "mailbox" ||
		control.request.ActorKey != "primary" ||
		control.request.IdempotencyKey != "send-1" {
		t.Fatalf("request = %+v", control.request)
	}
}

func TestHandleActorInputSendRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000113"
	firstAttempt := make(chan struct{})
	control := &actorInputSendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: workerapi.SendActorInputResponse{
			CorrelationID: correlationID,
			Completed:     &api.SendActorInputResponse{Sequence: 8},
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
		result <- task.handleActorInputSend(t.Context(), &runv0.ActorInputSendRequested{
			CorrelationId: correlationID,
			DeclaredId:    "mailbox",
			Address: &runv0.ActorInputSendRequested_ActorKey{
				ActorKey: "primary",
			},
			DataJson: `{"hello":"again"}`,
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
	assertRetriedWithStableFence(t, control.requests[0].Lease, control.requests[1].Lease, len(control.requests))
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
