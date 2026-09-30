package run

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestLiveSourceRequiresEnteredRunningExecution(t *testing.T) {
	now := pgvalue.Timestamptz(time.Now())
	valid := ExecutionAuthority{
		Run:      db.Run{ID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7()), DeploymentID: pgvalue.UUID(uuid.NewV7()), Status: db.RunStatusRunning, ActiveStartedAt: now},
		Attempt:  db.RunAttempt{Number: 2, EntrypointEnteredAt: now},
		Lease:    db.RunLease{Status: db.RunLeaseStatusRunning},
		Computer: db.LockRunLeaseClaimComputerRow{ID: pgvalue.UUID(uuid.NewV7())},
	}
	source, err := CheckLiveSource(valid, nil)
	if err != nil || source.RunID != valid.Run.ID || source.ComputerID != valid.Computer.ID || source.AttemptNumber != 2 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	for _, mutate := range []func(*ExecutionAuthority){
		func(a *ExecutionAuthority) { a.Run.Status = db.RunStatusWaiting },
		func(a *ExecutionAuthority) { a.Lease.Status = db.RunLeaseStatusCheckpointing },
		func(a *ExecutionAuthority) { a.Attempt.EntrypointEnteredAt.Valid = false },
		func(a *ExecutionAuthority) { a.Attempt.TerminalAt = now },
		func(a *ExecutionAuthority) { a.Lease.FinalizationOperationID = pgvalue.UUID(uuid.NewV7()) },
	} {
		a := valid
		mutate(&a)
		if _, err := CheckLiveSource(a, nil); !errors.Is(err, ErrStaleSource) {
			t.Fatalf("inactive execution=%v", err)
		}
	}
	if _, err := CheckLiveSource(valid, workergroup.ErrStaleClaims); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("worker claims=%v", err)
	}
	if _, err := CheckLiveSource(valid, pgx.ErrNoRows); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("missing source=%v", err)
	}
	failure := errors.New("database unavailable")
	if _, err := CheckLiveSource(valid, failure); !errors.Is(err, failure) {
		t.Fatalf("storage error hidden: %v", err)
	}
}
