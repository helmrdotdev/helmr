package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// startedSourceFixture claims and starts the fixture's source lease and adds a
// running Actor target. It enters the source only when enter is set.
func startedSourceFixture(t *testing.T, enter bool) (runtest.Fixture, runtest.RunLease, ExecutionFence, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	f, source, fence := executionClaimFixture(t)
	target := f.AddRunLease(t, "running", time.Now())
	targetID := f.ConvertToActor(t, t.Context(), target, `{"enabled":false}`)
	if _, err := claimExecutionTest(t, f, fence, true); err != nil {
		t.Fatal(err)
	}
	committedTx(t, f, func(tx pgx.Tx) error {
		started, err := StartExecution(t.Context(), tx, fence)
		if err != nil || !enter {
			return err
		}
		return EnterExecution(t.Context(), tx, fence, started.Run().EntrypointKind, started.Run().EntrypointDeclaredID)
	})
	var targetComputer pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, targetID).Scan(&targetComputer); err != nil {
		t.Fatal(err)
	}
	return f, source, fence, pgvalue.UUID(targetID), targetComputer
}

func committedTx(t *testing.T, f runtest.Fixture, fn func(pgx.Tx) error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = fn(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// rolledBackTx runs fn in a transaction that is always rolled back.
func rolledBackTx(t *testing.T, f runtest.Fixture, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	fn(tx)
}

func TestLiveExecutionPrologueStages(t *testing.T) {
	f, source, fence, _, _ := startedSourceFixture(t, true)
	missing := fence
	missing.LeaseID = pgvalue.UUID(uuid.NewV7())
	rolledBackTx(t, f, func(tx pgx.Tx) {
		if _, err := LocateLiveExecution(t.Context(), tx, missing); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("missing lease=%v", err)
		}
	})
	rolledBackTx(t, f, func(tx pgx.Tx) {
		locator, err := LocateLiveExecution(t.Context(), tx, fence)
		if err != nil {
			t.Fatal(err)
		}
		if locator.RunID() != pgvalue.UUID(source.RunID) || locator.AttemptNumber() != 1 || locator.EnvironmentID() != pgvalue.UUID(f.EnvironmentID) || locator.SessionID().Valid || !locator.ComputerID().Valid {
			t.Fatalf("locator Run=%s attempt=%d", pgvalue.UUIDString(locator.RunID()), locator.AttemptNumber())
		}
		secrets, err := locator.LockSecrets(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		execution, err := secrets.LockExecution(t.Context())
		if err != nil || execution.Run().ID != pgvalue.UUID(source.RunID) || execution.Computer().ID != locator.ComputerID() {
			t.Fatalf("execution Run=%s err=%v", pgvalue.UUIDString(execution.Run().ID), err)
		}
		if execution.DeliverySecrets() == nil {
			t.Fatal("the prologue's Secret locks are missing from the execution")
		}
	})
}

func TestSourceSecretsRequireLiveSource(t *testing.T) {
	f, _, fence, targetID, targetComputer := startedSourceFixture(t, false)
	for name, lock := range map[string]func(pgx.Tx) (SourceSecrets, error){
		"session": func(tx pgx.Tx) (SourceSecrets, error) {
			return LockSourceSecretsForSession(t.Context(), tx, fence, targetID)
		},
		"computer": func(tx pgx.Tx) (SourceSecrets, error) {
			return LockSourceSecretsForComputer(t.Context(), tx, fence, targetComputer)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rolledBackTx(t, f, func(tx pgx.Tx) {
				secrets, err := lock(tx)
				if err != nil {
					t.Fatal(err)
				}
				// The execution is running but has not entered its entrypoint.
				if _, _, err = secrets.LockLiveSource(t.Context()); !errors.Is(err, ErrStaleSource) {
					t.Fatalf("unentered source=%v", err)
				}
			})
		})
	}
}

func TestSourceSecretsStageOutcomes(t *testing.T) {
	f, source, fence, targetID, targetComputer := startedSourceFixture(t, true)
	missing := fence
	missing.LeaseID = pgvalue.UUID(uuid.NewV7())
	rolledBackTx(t, f, func(tx pgx.Tx) {
		if _, err := LockSourceSecretsForSession(t.Context(), tx, missing, targetID); !errors.Is(err, ErrStaleSource) {
			t.Fatalf("missing source lease=%v", err)
		}
	})
	rolledBackTx(t, f, func(tx pgx.Tx) {
		secrets, err := LockSourceSecretsForSession(t.Context(), tx, fence, pgvalue.UUID(uuid.NewV7()))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = secrets.LockLiveSource(t.Context()); err != ErrExecutionTargetNotFound {
			t.Fatalf("missing target=%v", err)
		}
	})
	for name, lock := range map[string]func(pgx.Tx) (SourceSecrets, error){
		"session": func(tx pgx.Tx) (SourceSecrets, error) {
			return LockSourceSecretsForSession(t.Context(), tx, fence, targetID)
		},
		"computer": func(tx pgx.Tx) (SourceSecrets, error) {
			return LockSourceSecretsForComputer(t.Context(), tx, fence, targetComputer)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rolledBackTx(t, f, func(tx pgx.Tx) {
				secrets, err := lock(tx)
				if err != nil {
					t.Fatal(err)
				}
				execution, live, err := secrets.LockLiveSource(t.Context())
				if err != nil || execution.Run().ID != pgvalue.UUID(source.RunID) || live.RunID() != pgvalue.UUID(source.RunID) {
					t.Fatalf("source Run=%s err=%v", pgvalue.UUIDString(live.RunID()), err)
				}
				if err = secrets.ValidateSourceDelivery(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestControlSecretsStageOutcomes(t *testing.T) {
	f, source, fence, targetID, targetComputer := startedSourceFixture(t, true)
	missing := fence
	missing.LeaseID = pgvalue.UUID(uuid.NewV7())
	rolledBackTx(t, f, func(tx pgx.Tx) {
		if _, err := LockControlSecrets(t.Context(), tx, missing, targetID); !errors.Is(err, ErrStaleSource) {
			t.Fatalf("missing source lease=%v", err)
		}
		if _, err := LockControlSecrets(t.Context(), tx, fence, pgvalue.UUID(uuid.NewV7())); err != ErrExecutionTargetNotFound {
			t.Fatalf("missing target=%v", err)
		}
	})
	for _, interrupt := range []bool{false, true} {
		rolledBackTx(t, f, func(tx pgx.Tx) {
			controls, err := LockControlSecrets(t.Context(), tx, fence, targetID)
			if err != nil {
				t.Fatal(err)
			}
			if target := controls.Target(); target.ID != targetID || target.ComputerID != targetComputer {
				t.Fatalf("target=%s Computer=%s", pgvalue.UUIDString(target.ID), pgvalue.UUIDString(target.ComputerID))
			}
			var live LiveSource
			if interrupt {
				_, live, _, err = controls.LockInterruptionLiveSource(t.Context())
			} else {
				_, live, err = controls.LockLiveSource(t.Context())
			}
			if err != nil || live.RunID() != pgvalue.UUID(source.RunID) {
				t.Fatalf("interrupt=%t source Run=%s err=%v", interrupt, pgvalue.UUIDString(live.RunID()), err)
			}
			if err = controls.Recheck(t.Context(), interrupt); err != nil {
				t.Fatalf("interrupt=%t recheck=%v", interrupt, err)
			}
		})
	}
}
