package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
)

func TestWorkerRunSourceRequiresEnteredRunningExecution(t *testing.T) {
	now := pgvalue.Timestamptz(time.Now())
	valid := run.ExecutionAuthority{
		Run:      db.Run{ID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7()), DeploymentID: pgvalue.UUID(uuid.NewV7()), Status: db.RunStatusRunning, ActiveStartedAt: now},
		Attempt:  db.RunAttempt{Number: 2, EntrypointEnteredAt: now},
		Lease:    db.RunLease{Status: db.RunLeaseStatusRunning},
		Computer: db.LockRunLeaseClaimComputerRow{ID: pgvalue.UUID(uuid.NewV7())},
	}
	source, err := validateWorkerRunSource(valid, nil)
	if err != nil || source.RunID != valid.Run.ID || source.ComputerID != valid.Computer.ID || source.AttemptNumber != 2 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	for _, mutate := range []func(*run.ExecutionAuthority){
		func(a *run.ExecutionAuthority) { a.Run.Status = db.RunStatusWaiting },
		func(a *run.ExecutionAuthority) { a.Lease.Status = db.RunLeaseStatusCheckpointing },
		func(a *run.ExecutionAuthority) { a.Attempt.EntrypointEnteredAt.Valid = false },
		func(a *run.ExecutionAuthority) { a.Attempt.TerminalAt = now },
		func(a *run.ExecutionAuthority) { a.Lease.FinalizationOperationID = pgvalue.UUID(uuid.NewV7()) },
	} {
		a := valid
		mutate(&a)
		if _, err := validateWorkerRunSource(a, nil); !errors.Is(err, errStaleWorkerRunSource) {
			t.Fatalf("inactive execution=%v", err)
		}
	}
	if _, err := validateWorkerRunSource(valid, run.ErrExecutionWorkerClaims); !errors.Is(err, errStaleWorkerClaims) {
		t.Fatalf("worker claims=%v", err)
	}
	if _, err := validateWorkerRunSource(valid, pgx.ErrNoRows); !errors.Is(err, errStaleWorkerRunSource) {
		t.Fatalf("missing source=%v", err)
	}
	failure := errors.New("database unavailable")
	if _, err := validateWorkerRunSource(valid, failure); !errors.Is(err, failure) {
		t.Fatalf("storage error hidden: %v", err)
	}
}
