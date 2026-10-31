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
		{"ordinary hold identity", "process_failed", f.computer, int64(1), nil, "23514"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,recovery_computer_id,recovery_lease_epoch,recovery_gap) VALUES($1,$2,$3,$4,'system',$5,$6,$7)`, f.env, uuid.NewV7(), f.session, test.kind, test.computer, test.epoch, test.details)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != test.code {
				t.Fatalf("invalid recovery identity accepted or wrong constraint: %v", err)
			}
		})
	}
	// Only an interrupt has a person, API key or Session issuer.
	for _, test := range []struct{ kind, issuer string }{{"interrupt", "system"}, {"process_failed", "user"}, {"storage_pressure", "session"}} {
		var issuerID any
		if test.issuer != "system" {
			issuerID = uuid.NewV7()
		}
		_, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,issuer_id) VALUES($1,$2,$3,$4,$5,$6)`, f.env, uuid.NewV7(), f.session, test.kind, test.issuer, issuerID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatalf("%s hold issued by %s accepted: %v", test.kind, test.issuer, err)
		}
	}
	// One unreleased process failure hold per Session.
	for range 2 {
		_, err := f.pool.Exec(t.Context(), `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, uuid.NewV7(), f.session)
		var pgerr *pgconn.PgError
		if err != nil && (!errors.As(err, &pgerr) || pgerr.Code != "23505") {
			t.Fatal(err)
		}
	}
	var held int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1 AND kind='process_failed' AND released_at IS NULL`, f.session).Scan(&held); err != nil || held != 1 {
		t.Fatalf("process failure holds %d %v", held, err)
	}
	// A recorded process failure always names its reason.
	_, err := f.pool.Exec(t.Context(), `UPDATE session_processes SET failure_recorded_at=clock_timestamp() WHERE session_id=$1`, f.session)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatalf("failure without reason accepted: %v", err)
	}
}
