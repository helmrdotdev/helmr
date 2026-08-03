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
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorOutputAppendControlPlane struct {
	*testRunLeaseControlPlane
	request      workerapi.AppendActorOutputRequest
	requests     []workerapi.AppendActorOutputRequest
	response     workerapi.AppendActorOutputResponse
	errors       []error
	firstAttempt chan struct{}
}

func (controlPlane *actorOutputAppendControlPlane) AppendActorOutput(
	_ context.Context,
	request workerapi.AppendActorOutputRequest,
) (workerapi.AppendActorOutputResponse, error) {
	controlPlane.request = request
	controlPlane.requests = append(controlPlane.requests, request)
	if len(controlPlane.errors) != 0 {
		err := controlPlane.errors[0]
		controlPlane.errors = controlPlane.errors[1:]
		if controlPlane.firstAttempt != nil {
			close(controlPlane.firstAttempt)
			controlPlane.firstAttempt = nil
		}
		return workerapi.AppendActorOutputResponse{}, err
	}
	return controlPlane.response, nil
}

func TestHandleActorOutputAppendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000112"
	controlPlane := &actorOutputAppendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.AppendActorOutputResponse{
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
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
		lease:        lease,
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
	if controlPlane.request.Lease != lease.Fence() ||
		string(controlPlane.request.Data) != `{"status":"working"}` ||
		controlPlane.request.ContentType != "application/json" ||
		controlPlane.request.IdempotencyKey != "output-1" {
		t.Fatalf("request = %+v", controlPlane.request)
	}
}

func TestHandleActorOutputAppendRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000114"
	firstAttempt := make(chan struct{})
	controlPlane := &actorOutputAppendControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		response: workerapi.AppendActorOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.ActorOutputRecord{
				ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Sequence: 9,
				Data: json.RawMessage(`{"status":"done"}`), ContentType: "application/json",
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
	if len(controlPlane.requests) != 2 {
		t.Fatalf("requests = %+v", controlPlane.requests)
	}
	assertRetriedWithStableFence(t, controlPlane.requests[0].Lease, controlPlane.requests[1].Lease, len(controlPlane.requests))
}
