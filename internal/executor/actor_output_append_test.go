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

type actorOutputAppendControl struct {
	*testRunLeaseControl
	request      api.WorkerAppendActorOutputRequest
	requests     []api.WorkerAppendActorOutputRequest
	response     api.WorkerAppendActorOutputResponse
	errors       []error
	firstAttempt chan struct{}
}

func (control *actorOutputAppendControl) AppendActorOutput(
	_ context.Context,
	request api.WorkerAppendActorOutputRequest,
) (api.WorkerAppendActorOutputResponse, error) {
	control.request = request
	control.requests = append(control.requests, request)
	if len(control.errors) != 0 {
		err := control.errors[0]
		control.errors = control.errors[1:]
		if control.firstAttempt != nil {
			close(control.firstAttempt)
			control.firstAttempt = nil
		}
		return api.WorkerAppendActorOutputResponse{}, err
	}
	return control.response, nil
}

func TestHandleActorOutputAppendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000112"
	control := &actorOutputAppendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerAppendActorOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.ActorOutputRecord{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34", Sequence: 8,
				Data: json.RawMessage(`{"status":"working"}`), ContentType: "application/json",
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
		result <- task.handleActorOutputAppend(t.Context(), &runv0.ActorOutputAppendRequested{
			CorrelationId:  correlationID,
			DataJson:       `{"status":"working"}`,
			ContentType:    "application/json",
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
	if control.request.Lease != lease.Fence() ||
		string(control.request.Data) != `{"status":"working"}` ||
		control.request.ContentType != "application/json" ||
		control.request.IdempotencyKey != "output-1" {
		t.Fatalf("request = %+v", control.request)
	}
}

func TestHandleActorOutputAppendRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000114"
	firstAttempt := make(chan struct{})
	control := &actorOutputAppendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerAppendActorOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.ActorOutputRecord{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Sequence: 9,
				Data: json.RawMessage(`{"status":"done"}`), ContentType: "application/json",
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
		result <- task.handleActorOutputAppend(t.Context(), &runv0.ActorOutputAppendRequested{
			CorrelationId: correlationID,
			DataJson:      `{"status":"done"}`,
			ContentType:   "application/json",
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
