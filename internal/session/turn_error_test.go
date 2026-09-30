package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/run"
)

// Session's message operations report a settling Turn as the durable
// turn_unsettled code and pass every other run Turn outcome through.
func TestTurnWorkErrorTranslatesUnsettledTurn(t *testing.T) {
	for _, err := range []error{run.ErrTurnUnsettled, fmt.Errorf("wrapped: %w", run.ErrTurnUnsettled)} {
		var operation *OperationError
		if !errors.As(turnWorkError(err), &operation) || operation.Code != "turn_unsettled" {
			t.Fatalf("turnWorkError(%v) = %v", err, turnWorkError(err))
		}
	}
	for _, err := range []error{nil, run.ErrTurnScope, run.ErrTurnNotActive, run.ErrTurnStopped, run.ErrWaitCursor} {
		if got := turnWorkError(err); got != err {
			t.Fatalf("turnWorkError(%v) = %v", err, got)
		}
	}
}
