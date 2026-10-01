package session

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestOutputEventReturnsIsolatedCopies(t *testing.T) {
	var output Output
	dbtest.FillSlices(t, &output.event)
	accessor := func() any { return output.Event() }
	// Snapshots are printed values, so they share no storage with the output.
	want := fmt.Sprintf("%#v", accessor())
	returned := reflect.New(reflect.TypeOf(accessor()))
	returned.Elem().Set(reflect.ValueOf(accessor()))
	dbtest.MutateSlices(t, returned.Interface())
	if fmt.Sprintf("%#v", returned.Elem().Interface()) == want {
		t.Fatal("mutation did not change the returned value")
	}
	if got := fmt.Sprintf("%#v", accessor()); got != want {
		t.Fatalf("mutating a returned value changed the next result:\ngot  %s\nwant %s", got, want)
	}
}

// Stale worker claims survive each worker operation's stale translation, so
// the worker re-authenticates instead of seeing a stale receipt.
func TestWorkerClaimsSurviveStaleTranslation(t *testing.T) {
	for name, translate := range map[string]func(error) error{
		"completion":  staleCompletion,
		"turn commit": staleTurnCommit,
		"output":      staleOutput,
	} {
		t.Run(name, func(t *testing.T) {
			if err := translate(workergroup.ErrStaleClaims); !errors.Is(err, workergroup.ErrStaleClaims) {
				t.Fatalf("claims error = %v", err)
			}
		})
	}
}

func TestStaleTranslations(t *testing.T) {
	if err := staleOutput(pgx.ErrNoRows); !errors.Is(err, ErrStaleOutput) || !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost output execution = %v", err)
	}
	if err := staleOutput(nil); err != ErrStaleOutput {
		t.Fatalf("changed output execution = %v", err)
	}
	if err := staleExecution(pgx.ErrNoRows); err != ErrStaleExecution {
		t.Fatalf("lost execution = %v", err)
	}
	unrelated := errors.New("database failed")
	if err := staleExecution(unrelated); err != unrelated {
		t.Fatalf("unrelated execution error = %v", err)
	}
	if err := staleCompletion(pgx.ErrNoRows); err != ErrStaleCompletion {
		t.Fatalf("lost completion = %v", err)
	}
	if err := staleCompletion(unrelated); err != unrelated {
		t.Fatalf("unrelated completion error = %v", err)
	}
}

// The Turn and wait-cursor sentinels come from run; a Turn commit keeps the
// outcome it gave each of them.
func TestTurnCommitStaleBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		stale bool
	}{
		{"scope", run.ErrTurnScope, true},
		{"not active", run.ErrTurnNotActive, true},
		{"stopped", run.ErrTurnStopped, true},
		{"unsettled", run.ErrTurnUnsettled, false},
		{"wait cursor", run.ErrWaitCursor, false},
		{"Session input authority", ErrAuthority, false},
		{"stale execution", ErrStaleExecution, true},
		{"stale run lease", run.ErrStale, false},
		{"missing row", pgx.ErrNoRows, true},
		{"unrelated", errors.New("database failed"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, err := range []error{test.err, fmt.Errorf("wrapped: %w", test.err)} {
				if got := errors.Is(staleTurnCommit(err), ErrStaleTurnCommit); got != test.stale {
					t.Fatalf("turn commit stale(%v) = %v", err, got)
				}
			}
		})
	}
	rejection := &OperationError{Code: "turn_unsettled"}
	joined := staleTurnCommit(rejection)
	var operation *OperationError
	if !errors.Is(joined, ErrStaleTurnCommit) || !errors.As(joined, &operation) || operation != rejection {
		t.Fatalf("rejected commit = %v", joined)
	}
}

func TestTurnScopeRejectsUnaddressedTurnBeforeGeneration(t *testing.T) {
	var authority run.Execution
	if _, err := turnScope(authority, TurnWork{RunGeneration: 0}); err != errTurnReference {
		t.Fatalf("unaddressed Turn = %v", err)
	}
	if _, err := turnScope(authority, TurnWork{TurnID: uuid.NewV7()}); err != run.ErrTurnScope {
		t.Fatalf("missing generation = %v", err)
	}
}

func TestDecideTerminal(t *testing.T) {
	tests := []struct {
		name       string
		state      terminalState
		completion ActorCompletion
		want       terminalDecision
	}{
		{
			name: "successful progress remains open",
			state: func() terminalState {
				s := actorTerminalState("open", 2, 4)
				s.session.CommittedInputSequence = 3
				return s
			}(),
			completion: ActorCompletion{Kind: ActorSucceeded},
			want:       terminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "input arriving after idle admission belongs to continuation",
			state: func() terminalState {
				s := actorTerminalState("open", 2, 2)
				s.session.NextInputSequence = 4
				return s
			}(),
			completion: ActorCompletion{Kind: ActorSucceeded},
			want:       terminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "open"},
		},
		{
			name: "late close frontier does not make an idle return fail",
			state: func() terminalState {
				s := actorTerminalState("closing", 2, 2)
				s.session.NextInputSequence = 4
				s.session.CloseSequence = pgtype.Int8{Int64: 3, Valid: true}
				return s
			}(),
			completion: ActorCompletion{Kind: ActorSucceeded},
			want:       terminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closing"},
		},
		{
			name: "admission backlog without progress fails before close",
			state: func() terminalState {
				s := actorTerminalState("closing", 2, 4)
				s.session.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return s
			}(),
			completion: ActorCompletion{Kind: ActorSucceeded},
			want:       terminalDecision{runStatus: db.RunStatusFailed, actorStatus: "closing", runReason: pgvalue.Text("no_progress")},
		},
		{
			name: "closing waits for process reconciliation",
			state: func() terminalState {
				s := actorTerminalState("closing", 2, 2)
				s.session.CloseSequence = pgtype.Int8{Int64: 2, Valid: true}
				return s
			}(),
			completion: ActorCompletion{Kind: ActorSucceeded},
			want:       terminalDecision{runStatus: db.RunStatusSucceeded, actorStatus: "closing"},
		},
		{
			name:       "runtime failure rolls cursor back",
			state:      actorTerminalState("open", 2, 4),
			completion: ActorCompletion{Kind: ActorFailed},
			want:       terminalDecision{runStatus: db.RunStatusFailed, runReason: pgvalue.Text("actor_failed"), actorStatus: "open"},
		},
		{
			name:       "interruption cancels the Run",
			state:      actorTerminalState("open", 2, 4),
			completion: ActorCompletion{Kind: ActorInterrupted},
			want:       terminalDecision{runStatus: db.RunStatusCancelled, runReason: pgvalue.Text("session_interrupted"), actorStatus: "open"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideTerminal(test.state, test.completion)
			if got != test.want {
				t.Fatalf("decision = %#v, want %#v", got, test.want)
			}
		})
	}
}

func actorTerminalState(status string, start, highWatermark int64) terminalState {
	return terminalState{
		session: db.Session{Status: status, CommittedInputSequence: start, NextInputSequence: highWatermark + 1},
		run: db.Run{
			SessionInputStartSequence: pgtype.Int8{Int64: start, Valid: true},
			SessionInputHighWatermark: pgtype.Int8{Int64: highWatermark, Valid: true},
		},
	}
}

func TestRunFailureBodies(t *testing.T) {
	interrupted, err := runFailure("session_interrupted", "Session execution was interrupted")
	if err != nil || string(interrupted) != `{"code":"session_interrupted","message":"Session execution was interrupted","details":{}}` {
		t.Fatalf("interrupted failure = %s %v", interrupted, err)
	}
	failed, err := runFailureFromCompletion("actor_failed", []byte(`{"details":{"step":1},"message":"failed"}`))
	if err != nil || string(failed) != `{"code":"actor_failed","message":"failed","details":{"step":1}}` {
		t.Fatalf("completion failure = %s %v", failed, err)
	}
	defaulted, err := runFailureFromCompletion("actor_failed", []byte(`{"message":"failed"}`))
	if err != nil || string(defaulted) != `{"code":"actor_failed","message":"failed","details":{}}` {
		t.Fatalf("completion failure without details = %s %v", defaulted, err)
	}
	for _, raw := range []string{`{}`, `{"message":""}`, `{"message":"failed","details":[]}`, `{`} {
		if _, err := runFailureFromCompletion("actor_failed", []byte(raw)); err == nil {
			t.Fatalf("invalid completion failure %s was accepted", raw)
		}
	}
}
