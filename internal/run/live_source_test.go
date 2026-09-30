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
	valid := Execution{
		run:     db.Run{ID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7()), DeploymentID: pgvalue.UUID(uuid.NewV7()), Status: db.RunStatusRunning, ActiveStartedAt: now},
		attempt: db.RunAttempt{Number: 2, EntrypointEnteredAt: now},
		lease:   db.RunLease{Status: db.RunLeaseStatusRunning},
	}
	source, err := valid.LiveSource()
	if err != nil || source.RunID() != valid.run.ID || source.OrgID() != valid.run.OrgID || source.ProjectID() != valid.run.ProjectID || source.EnvironmentID() != valid.run.EnvironmentID || source.DeploymentID() != valid.run.DeploymentID || source.AttemptNumber() != 2 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	for _, mutate := range []func(*Execution){
		func(a *Execution) { a.run.Status = db.RunStatusWaiting },
		func(a *Execution) { a.lease.Status = db.RunLeaseStatusCheckpointing },
		func(a *Execution) { a.attempt.EntrypointEnteredAt.Valid = false },
		func(a *Execution) { a.attempt.TerminalAt = now },
		func(a *Execution) { a.lease.FinalizationOperationID = pgvalue.UUID(uuid.NewV7()) },
	} {
		a := valid
		mutate(&a)
		if _, err := checkLiveSource(a, nil); !errors.Is(err, ErrStaleSource) {
			t.Fatalf("inactive execution=%v", err)
		}
	}
	if _, err := checkLiveSource(valid, workergroup.ErrStaleClaims); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("worker claims=%v", err)
	}
	if _, err := checkLiveSource(valid, pgx.ErrNoRows); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("missing source=%v", err)
	}
	failure := errors.New("database unavailable")
	if _, err := checkLiveSource(valid, failure); !errors.Is(err, failure) {
		t.Fatalf("storage error hidden: %v", err)
	}
}
