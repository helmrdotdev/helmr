package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRecoveryHoldIdentityRequiresItsSessionComputerAndLease(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		name, kind string
		computer   any
		epoch      any
		details    any
		code       string
	}{
		{"missing computer", "recovery_gap", nil, int64(1), `{}`, "23514"},
		{"missing epoch", "recovery_gap", f.computer, nil, `{}`, "23514"},
		{"nonpositive epoch", "recovery_gap", f.computer, int64(0), `{}`, "23514"},
		{"missing details", "recovery_gap", f.computer, int64(1), nil, "23514"},
		{"unknown lease", "recovery_gap", f.computer, int64(2), `{}`, "23503"},
		{"other computer", "recovery_gap", uuid.NewV7(), int64(1), `{}`, "23503"},
		{"ordinary hold identity", "control", f.computer, int64(1), nil, "23514"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,kind,recovery_computer_id,recovery_lease_epoch,recovery_gap) VALUES($1,$2,$3,'local','test',$4,$5,$6,$7)`, f.env, uuid.NewV7(), f.session, test.kind, test.computer, test.epoch, test.details)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != test.code {
				t.Fatalf("invalid recovery identity accepted or wrong constraint: %v", err)
			}
		})
	}
}
