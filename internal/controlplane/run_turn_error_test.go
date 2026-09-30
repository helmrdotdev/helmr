package controlplane

import (
	"errors"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
)

// The Turn and wait-cursor sentinels come from run; each worker boundary keeps
// the outcome it gave the equivalent session error.
func TestRunTurnSentinelBoundaries(t *testing.T) {
	sentinels := []error{run.ErrTurnScope, run.ErrTurnNotActive, run.ErrTurnStopped, run.ErrTurnUnsettled, run.ErrWaitCursor}
	unrelated := errors.New("database failed")
	for _, test := range []struct {
		boundary string
		err      error
		// failure is the 200 failure code, or "" when the error is not a
		// semantic failure.
		failure                                 string
		staleTimer, staleInput, staleTurnCommit bool
	}{
		{"scope", run.ErrTurnScope, "stale_execution", true, true, true},
		{"not active", run.ErrTurnNotActive, "turn_not_active", false, false, true},
		{"stopped", run.ErrTurnStopped, "turn_stopping", true, true, true},
		{"unsettled", run.ErrTurnUnsettled, "turn_unsettled", false, false, false},
		{"wait cursor", run.ErrWaitCursor, "", true, true, false},
		{"Session input authority", session.ErrAuthority, "", false, true, false},
		{"stale lease", errStaleRunLeaseClaim, "", true, true, true},
		{"unrelated", unrelated, "", false, false, false},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			for _, err := range []error{test.err, fmt.Errorf("wrapped: %w", test.err)} {
				failure, ok := actorOutputAppendFailure(err)
				if ok != (test.failure != "") || failure.Code != test.failure || failure.Retryable {
					t.Fatalf("actor output failure(%v) = %+v, %v", err, failure, ok)
				}
				if got := staleTimerWait(err); got != test.staleTimer {
					t.Fatalf("timer wait stale(%v) = %v", err, got)
				}
				if got := staleActorInputWait(err); got != test.staleInput {
					t.Fatalf("Actor input wait stale(%v) = %v", err, got)
				}
				if got := errors.Is(staleActorTurnCommit(err), errStaleActorTurnCommit); got != test.staleTurnCommit {
					t.Fatalf("turn commit stale(%v) = %v", err, got)
				}
			}
		})
	}
	for _, sentinel := range sentinels {
		for _, other := range sentinels {
			if sentinel != other && errors.Is(sentinel, other) {
				t.Fatalf("%v matches %v", sentinel, other)
			}
		}
	}
}
