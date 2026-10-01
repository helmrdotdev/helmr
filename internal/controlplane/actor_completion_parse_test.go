package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestParseActorCompletionRequestBindsGenerationAndOperation(t *testing.T) {
	operationID := uuid.NewV7().String()
	request := workerapi.CompleteActorRequest{
		Lease: workerapi.RunLeaseFence{ID: uuid.NewV7().String(), LeaseSequence: 1},
		Outcome: workerapi.ActorOutcome{
			RunGeneration: 1,
			Succeeded:     &workerapi.ActorSucceeded{},
		},
		OperationID: operationID,
	}
	parsed, err := parseActorCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.completion.Kind != session.ActorSucceeded || parsed.completion.OperationID.String() != operationID || parsed.completion.Fingerprint == "" || parsed.completion.RunGeneration != 1 {
		t.Fatalf("parsed Actor completion = %#v", parsed)
	}
}

func TestParseActorCompletionRejectsNoncanonicalFailureMessage(t *testing.T) {
	operationID := uuid.NewV7().String()
	request := workerapi.CompleteActorRequest{
		Lease: workerapi.RunLeaseFence{ID: uuid.NewV7().String(), LeaseSequence: 1},
		Outcome: workerapi.ActorOutcome{
			RunGeneration: 1,
			Failed:        &workerapi.TaskFailure{Message: " failed "},
		},
		OperationID: operationID,
	}
	if _, err := parseActorCompletionRequest(request); err == nil {
		t.Fatal("Actor completion with noncanonical failure message was accepted")
	}
}

func TestActorContinuationHonorsManualCancellation(t *testing.T) {
	actor := db.Session{Status: "open", CommittedInputSequence: 2, NextInputSequence: 5}
	if !session.CanStartContinuation(actor) {
		t.Fatal("backlogged open Actor should need a continuation")
	}
	actor.DispatchHoldID = pgvalue.UUID(uuid.NewV7())
	if session.CanStartContinuation(actor) {
		t.Fatal("manual Run cancellation hold admitted a continuation")
	}
}
