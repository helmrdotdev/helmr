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
		Lease: lease.Fence(), CorrelationID: uuid.NewV7().String(),
		Data: json.RawMessage(`{"b":2,"a":1}`), ContentType: " application/json ",
		IdempotencyKey: "output-1",
	}
	parsed, err := parseWorkerActorOutputAppend(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.lease.leaseID.String() != lease.ID ||
		parsed.correlationID.String() != request.CorrelationID ||
		string(parsed.data) != `{"a":1,"b":2}` ||
		parsed.contentType != "application/json" ||
		parsed.idempotencyKey != "output-1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	request.Data = json.RawMessage(`{`)
	if _, err := parseWorkerActorOutputAppend(request); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	request.Data = json.RawMessage(`null`)
	request.ContentType = " "
	if _, err := parseWorkerActorOutputAppend(request); err == nil {
		t.Fatal("empty normalized content type was accepted")
	}
	request.ContentType = "application/json"
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
		{errActorOutputUnavailable, "actor_not_open"},
		{errActorOutputAppendConflict, "actor_output_conflict"},
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
