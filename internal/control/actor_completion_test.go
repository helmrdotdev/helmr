package control

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseActorCompletionRequestBindsCursorAndWorkspaceProof(t *testing.T) {
	taskRequest := validTaskCompletionRequest(t)
	request := api.WorkerCompleteActorRequest{
		Lease: taskRequest.Lease,
		Outcome: api.WorkerActorOutcome{
			TerminalInputSequence: 0,
			Succeeded:             &api.WorkerActorSucceeded{},
		},
		Workspace: taskRequest.Workspace,
	}
	parsed, err := parseActorCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != actorCompletionSucceeded || parsed.terminalInputSequence != 0 || parsed.capture == nil || parsed.rollback != nil || parsed.fingerprint == "" {
		t.Fatalf("parsed Actor completion = %#v", parsed)
	}
}

func TestDecideActorRunTerminal(t *testing.T) {
	now := time.Date(2026, time.July, 22, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name       string
		authority  runLeaseClaimAuthority
		completion parsedActorCompletion
		want       actorRunTerminalDecision
	}{
		{
			name:       "successful progress remains open",
			authority:  actorTerminalAuthority("open", 2, 4),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded, terminalInputSequence: 3},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorState: "open", commitCursor: true},
		},
		{
			name: "admission backlog without progress fails before close",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 4)
				a.actor.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded, terminalInputSequence: 2},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorState: "failed", failureCode: pgvalue.Text("no-progress"), commitCursor: true},
		},
		{
			name: "closing at committed boundary closes",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 2)
				a.actor.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded, terminalInputSequence: 2},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorState: "closed", commitCursor: true},
		},
		{
			name:       "runtime failure rolls cursor back",
			authority:  actorTerminalAuthority("open", 2, 4),
			completion: parsedActorCompletion{kind: actorCompletionFailed, terminalInputSequence: 3},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"), actorState: "failed", failureCode: pgvalue.Text("run-failed")},
		},
		{
			name: "due expiry overrides runtime failure",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("open", 2, 4)
				a.actor.ExpiresAt = pgvalue.Timestamptz(now)
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionFailed, terminalInputSequence: 3},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"), actorState: "expired"},
		},
		{
			name: "due expiry prevents successor",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("open", 2, 2)
				a.actor.ExpiresAt = pgvalue.Timestamptz(now)
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded, terminalInputSequence: 2},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorState: "expired", commitCursor: true},
		},
		{
			name: "accepted close remains authoritative after expiry",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 4)
				a.actor.CloseSequence = pgtype.Int8{Int64: 4, Valid: true}
				a.actor.ExpiresAt = pgvalue.Timestamptz(now)
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionSucceeded, terminalInputSequence: 3},
			want:       actorRunTerminalDecision{runStatus: db.RunStatusSucceeded, actorState: "closing", commitCursor: true},
		},
		{
			name: "runtime failure after accepted close is not expiry",
			authority: func() runLeaseClaimAuthority {
				a := actorTerminalAuthority("closing", 2, 4)
				a.actor.CloseSequence = pgtype.Int8{Int64: 4, Valid: true}
				a.actor.ExpiresAt = pgvalue.Timestamptz(now)
				return a
			}(),
			completion: parsedActorCompletion{kind: actorCompletionFailed, terminalInputSequence: 3},
			want: actorRunTerminalDecision{
				runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"),
				actorState: "failed", failureCode: pgvalue.Text("run-failed"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideActorRunTerminal(test.authority, test.completion, now)
			if got != test.want {
				t.Fatalf("decision = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestActorCompletionDoesNotRetryExpiredActor(t *testing.T) {
	now := time.Date(2026, time.July, 22, 1, 2, 3, 0, time.UTC)
	actor := db.Actor{State: "open", ExpiresAt: pgvalue.Timestamptz(now)}
	_, retry, err := actorCompletionRetryAt(db.Run{}, db.RunAttempt{}, actor, parsedActorCompletion{kind: actorCompletionFailed}, now)
	if err != nil {
		t.Fatal(err)
	}
	if retry {
		t.Fatal("expired Actor scheduled a retry")
	}

	actor.State = "closing"
	run := db.Run{
		RetryPolicy: []byte(`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`),
	}
	retryAt, retry, err := actorCompletionRetryAt(
		run,
		db.RunAttempt{Number: 1},
		actor,
		parsedActorCompletion{kind: actorCompletionFailed},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !retry || !retryAt.Equal(now.Add(time.Millisecond)) {
		t.Fatalf("closing Actor retry = %s, %t", retryAt, retry)
	}
}

func actorTerminalAuthority(state string, start, highWatermark int64) runLeaseClaimAuthority {
	return runLeaseClaimAuthority{
		actor: db.Actor{State: state},
		run: db.Run{
			ActorStartInputSequence:      pgtype.Int8{Int64: start, Valid: true},
			ActorStartInputHighWatermark: pgtype.Int8{Int64: highWatermark, Valid: true},
		},
	}
}

func TestActorNeedsContinuationHonorsManualCancellation(t *testing.T) {
	actor := db.Actor{State: "open", CommittedInputSequence: 2, NextInputSequence: 5}
	if !actorNeedsContinuation(actor) {
		t.Fatal("backlogged open Actor should need a continuation")
	}
	actor.ManualRunCancelled = true
	if actorNeedsContinuation(actor) {
		t.Fatal("manual Run cancellation hold admitted a continuation")
	}
}
