package control

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/idempotency"
)

func TestParseWorkerActorInputSendRequiresExactAddress(t *testing.T) {
	lease := validRunLeaseReceipt(uuid.Must(uuid.NewV7()))
	request := api.WorkerSendActorInputRequest{
		Lease: lease, CorrelationID: uuid.Must(uuid.NewV7()).String(),
		ActorDeclaredID: "mailbox", ActorKey: "primary",
		Input: json.RawMessage(`{"hello":"world"}`), IdempotencyKey: "send-1",
	}
	parsed, err := parseWorkerActorInputSend(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.lease.runID.String() != lease.RunID ||
		parsed.correlationID.String() != request.CorrelationID ||
		parsed.idempotencyKey != "send-1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	request.ActorID = "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := parseWorkerActorInputSend(request); err == nil {
		t.Fatal("both Actor address variants were accepted")
	}
	request.ActorID = ""
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

func TestAuthorizeActorInputSendSourceRequiresExactLiveReceipt(t *testing.T) {
	_, store, worker, turnRequest, _ := newActorTurnCommitFixture(t)
	request := api.WorkerSendActorInputRequest{Lease: turnRequest.Lease}
	parsedLease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parsedWorkerActorInputSend{lease: parsedLease}
	if err := authorizeActorInputSendSource(
		t.Context(),
		store,
		worker,
		request,
		parsed,
		store.renewal.EnvironmentID,
	); err != nil {
		t.Fatal(err)
	}
	request.Lease.WriterGeneration++
	if err := authorizeActorInputSendSource(
		t.Context(),
		store,
		worker,
		request,
		parsed,
		store.renewal.EnvironmentID,
	); !errors.Is(err, errStaleActorInputSend) {
		t.Fatalf("altered receipt error = %v", err)
	}
}
