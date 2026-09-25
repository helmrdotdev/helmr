package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseActorCompletionRequestBindsGenerationAndWorkspaceProof(t *testing.T) {
	taskRequest := validTaskCompletionRequest(t)
	request := workerapi.CompleteActorRequest{
		Lease: taskRequest.Lease,
		Outcome: workerapi.ActorOutcome{
			RunGeneration: 1,
			Succeeded:     &workerapi.ActorSucceeded{},
		},
		Workspace: taskRequest.Workspace,
	}
	parsed, err := parseActorCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != actorCompletionSucceeded || parsed.capture == nil || parsed.fingerprint == "" {
		t.Fatalf("parsed Actor completion = %#v", parsed)
	}
}

func TestParseActorCompletionRejectsNoncanonicalFailureMessage(t *testing.T) {
	taskRequest := validTaskCompletionRequest(t)
	request := workerapi.CompleteActorRequest{
		Lease: taskRequest.Lease,
		Outcome: workerapi.ActorOutcome{
			RunGeneration: 1,
			Failed:        &workerapi.TaskFailure{Message: " failed "},
		},
		Workspace: workerapi.TaskWorkspaceProof{
			Captured: taskRequest.Workspace.Captured,
		},
	}
	if _, err := parseActorCompletionRequest(request); err == nil {
		t.Fatal("Actor completion with noncanonical failure message was accepted")
	}
}

func TestDecideActorRunTerminal(t *testing.T) {
	tests := []struct {
		name       string
		authority  runLeaseClaimAuthority
		completion parsedActorCompletion
		want       actorRunTerminalDecision
	}{
		{
			name: "successful progress remains open",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("open", 2, 4)
				a.actor.CommittedInputSequence = 3
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "input arriving after idle admission belongs to continuation",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("open", 2, 2)
				a.actor.NextInputSequence = 4
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "late close frontier does not make an idle return fail",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 2)
				a.actor.NextInputSequence = 4
				a.actor.CloseSequence = pgtype.Int8{Int64: 3, Valid: true}
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closing"},
		},
		{
			name: "admission backlog without progress fails before close",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 4)
				a.actor.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, actorStatus: "closing", runReason: pgvalue.Text("no_progress")},
		},
		{
			name: "closing at committed boundary closes",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 2)
				a.actor.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closed"},
		},
		{
			name:       "runtime failure rolls cursor back",
			authority:  actorTerminalAuthority("open", 2, 4),
			completion: parsedActorCompletion{kind: actorCompletionFailed},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"), actorStatus: "open"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideActorRunTerminal(test.authority, test.completion)
			if got != test.want {
				t.Fatalf("decision = %#v, want %#v", got, test.want)
			}
		})
	}
}

func actorTerminalAuthority(state string, start, highWatermark int64) runLeaseClaimAuthority {
	return runLeaseClaimAuthority{
		actor: db.Session{Status: state, CommittedInputSequence: start, NextInputSequence: highWatermark + 1},
		run: db.Run{
			SessionInputStartSequence: pgtype.Int8{Int64: start, Valid: true},
			SessionInputHighWatermark: pgtype.Int8{Int64: highWatermark, Valid: true},
		},
	}
}

func TestActorNeedsContinuationHonorsManualCancellation(t *testing.T) {
	actor := db.Session{Status: "open", CommittedInputSequence: 2, NextInputSequence: 5}
	if !actorNeedsContinuation(actor) {
		t.Fatal("backlogged open Actor should need a continuation")
	}
	actor.DispatchHoldID = pgvalue.UUID(uuid.NewV7())
	if actorNeedsContinuation(actor) {
		t.Fatal("manual Run cancellation hold admitted a continuation")
	}
}
