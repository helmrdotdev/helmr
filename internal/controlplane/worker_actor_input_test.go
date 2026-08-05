package controlplane

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestParseWorkerActorInputSendRequiresSessionID(t *testing.T) {
	lease := validRunLeaseAssignment(uuid.Must(uuid.NewV7()))
	request := workerapi.SendActorInputRequest{
		Lease: lease.Fence(), CorrelationID: uuid.Must(uuid.NewV7()).String(),
		SessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
		Input:     json.RawMessage(`{"hello":"world"}`), IdempotencyKey: "send-1",
	}
	parsed, err := parseWorkerActorInputSend(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.lease.leaseID.String() != lease.ID ||
		parsed.correlationID.String() != request.CorrelationID ||
		parsed.idempotencyKey != "send-1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	request.SessionID = ""
	if _, err := parseWorkerActorInputSend(request); err == nil {
		t.Fatal("missing Session ID was accepted")
	}
	request.SessionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
	request.IdempotencyKey = " send-1 "
	if _, err := parseWorkerActorInputSend(request); err == nil {
		t.Fatal("padded idempotency key was accepted")
	}
	request.IdempotencyKey = string(make([]byte, maxIdempotencyKeyLength+1))
	if _, err := parseWorkerActorInputSend(request); err == nil {
		t.Fatal("oversized idempotency key was accepted")
	}
}

func TestActorInputSendFailurePreservesSemanticCodes(t *testing.T) {
	conflict := idempotency.ConflictError{ClaimID: uuid.Must(uuid.NewV7())}
	tests := []struct {
		err  error
		code string
	}{
		{conflict, "idempotency_conflict"},
		{errActorInputTooLarge, "actor_input_too_large"},
		{errActorSequenceExhausted, "actor_sequence_exhausted"},
		{errActorInputUnavailable, "actor_not_open"},
		{errActorInputAppendConflict, "actor_input_conflict"},
	}
	for _, test := range tests {
		failure, ok := actorInputSendFailure(test.err)
		if !ok || failure.Code != test.code || failure.Retryable {
			t.Fatalf("failure(%v) = %+v, %v", test.err, failure, ok)
		}
	}
	if _, ok := actorInputSendFailure(errors.New("database failed")); ok {
		t.Fatal("infrastructure failure was exposed as a semantic result")
	}
}

func TestAuthorizeActorInputSendSourceRequiresExactLiveFence(t *testing.T) {
	_, store, worker, turnRequest, _ := newActorTurnCommitFixture(t)
	request := workerapi.SendActorInputRequest{Lease: turnRequest.Lease}
	if err := authorizeActorInputSendSource(
		t.Context(),
		store,
		worker,
		request,
		store.renewal.EnvironmentID,
	); err != nil {
		t.Fatal(err)
	}
	request.Lease.LeaseSequence++
	if err := authorizeActorInputSendSource(
		t.Context(),
		store,
		worker,
		request,
		store.renewal.EnvironmentID,
	); !errors.Is(err, errStaleActorInputSend) {
		t.Fatalf("altered fence error = %v", err)
	}
}
