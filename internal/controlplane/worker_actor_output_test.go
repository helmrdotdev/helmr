package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestParseWorkerActorOutputAppendNormalizesPayload(t *testing.T) {
	lease := validRunLeaseAssignment(uuid.NewV7())
	request := workerapi.AppendActorOutputRequest{
		TurnID: uuid.NewV7().String(), RunGeneration: 1,
		Lease: lease.Fence(), CorrelationID: uuid.NewV7().String(),
		Data:           json.RawMessage(`{"b":2,"a":1}`),
		IdempotencyKey: "output-1",
	}
	parsed, err := parseWorkerActorOutputAppend(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.lease.leaseID.String() != lease.ID ||
		parsed.correlationID.String() != request.CorrelationID ||
		string(parsed.data) != `{"a":1,"b":2}` ||
		parsed.idempotencyKey != "output-1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	request.Data = json.RawMessage(`{`)
	if _, err := parseWorkerActorOutputAppend(request); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	request.Data = json.RawMessage(`null`)
	request.RunGeneration = 0
	if _, err := parseWorkerActorOutputAppend(request); err == nil {
		t.Fatal("missing execution generation was accepted")
	}
	request.RunGeneration = 1
	request.IdempotencyKey = " output-1 "
	if _, err := parseWorkerActorOutputAppend(request); err == nil {
		t.Fatal("padded idempotency key was accepted")
	}
}

func TestActorOutputAppendFailurePreservesSemanticCodes(t *testing.T) {
	conflict := idempotency.ConflictError{ClaimID: uuid.NewV7()}
	tests := []struct {
		err  error
		code string
	}{
		{conflict, "idempotency_conflict"},
		{errActorOutputTooLarge, "actor_output_too_large"},
		{errActorSequenceExhausted, "actor_sequence_exhausted"},
	}
	for _, test := range tests {
		failure, ok := actorOutputAppendFailure(test.err)
		if !ok || failure.Code != test.code || failure.Retryable {
			t.Fatalf("failure(%v) = %+v, %v", test.err, failure, ok)
		}
	}
	if _, ok := actorOutputAppendFailure(errors.New("database failed")); ok {
		t.Fatal("infrastructure failure was exposed as a semantic result")
	}
}
