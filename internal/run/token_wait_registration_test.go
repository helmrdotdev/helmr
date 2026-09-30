package run

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestTokenWaitRegistrationNamesTheRejectingLock(t *testing.T) {
	failed := errors.New("statement failed")
	for _, test := range []struct {
		err  error
		want string
	}{
		{tokenWaitHostError(&workergroup.ExecutionHostError{}), "lock worker group"},
		{tokenWaitHostError(&workergroup.ExecutionHostError{Err: failed}), "lock worker group: statement failed"},
		{tokenWaitHostError(&workergroup.ExecutionHostError{Host: true}), "lock current worker epoch"},
		{tokenWaitHostError(&workergroup.ExecutionHostError{Host: true, Err: pgx.ErrNoRows}), "lock current worker epoch: no rows in result set"},
		{tokenWaitInstanceError(&computer.TokenWaitInstanceError{}), "lock active Computer"},
		{tokenWaitInstanceError(&computer.TokenWaitInstanceError{Err: failed}), "lock active Computer: statement failed"},
		{tokenWaitInstanceError(&computer.TokenWaitInstanceError{Instance: true}), "lock ready runtime"},
		{tokenWaitInstanceError(&computer.TokenWaitInstanceError{Instance: true, Err: failed}), "lock ready runtime: statement failed"},
	} {
		if test.err.Error() != test.want {
			t.Errorf("registration error = %q, want %q", test.err, test.want)
		}
	}
}
