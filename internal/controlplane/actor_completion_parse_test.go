package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
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
	if parsed.kind != actorCompletionSucceeded || parsed.operationID.String() != operationID || parsed.fingerprint == "" {
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

func TestDecideActorRunTerminal(t *testing.T) {
	tests := []struct {
		name       string
		state      actorRunTerminalState
		completion parsedActorCompletion
		want       actorRunTerminalDecision
	}{
		{
			name: "successful progress remains open",
			state: func() actorRunTerminalState {
				s := actorTerminalState("open", 2, 4)
				s.session.CommittedInputSequence = 3
				return s
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "input arriving after idle admission belongs to continuation",
			state: func() actorRunTerminalState {
				s := actorTerminalState("open", 2, 2)
				s.session.NextInputSequence = 4
				return s
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "late close frontier does not make an idle return fail",
			state: func() actorRunTerminalState {
				s := actorTerminalState("closing", 2, 2)
				s.session.NextInputSequence = 4
				s.session.CloseSequence = pgtype.Int8{Int64: 3, Valid: true}
				return s
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closing"},
		},
		{
			name: "admission backlog without progress fails before close",
			state: func() actorRunTerminalState {
				s := actorTerminalState("closing", 2, 4)
				s.session.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return s
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, actorStatus: "closing", runReason: pgvalue.Text("no_progress")},
		},
		{
			name: "closing waits for process reconciliation",
			state: func() actorRunTerminalState {
				s := actorTerminalState("closing", 2, 2)
				s.session.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return s
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closing"},
		},
		{
			name:       "runtime failure rolls cursor back",
			state:      actorTerminalState("open", 2, 4),
			completion: parsedActorCompletion{kind: actorCompletionFailed},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"), actorStatus: "open"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideActorRunTerminal(test.state, test.completion)
			if got != test.want {
				t.Fatalf("decision = %#v, want %#v", got, test.want)
			}
		})
	}
}

func actorTerminalState(status string, start, highWatermark int64) actorRunTerminalState {
	return actorRunTerminalState{
		session: db.Session{Status: status, CommittedInputSequence: start, NextInputSequence: highWatermark + 1},
		run: db.Run{
			SessionInputStartSequence: pgtype.Int8{Int64: start, Valid: true},
			SessionInputHighWatermark: pgtype.Int8{Int64: highWatermark, Valid: true},
		},
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
