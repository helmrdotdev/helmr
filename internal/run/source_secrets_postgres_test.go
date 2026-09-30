package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
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
	var sourceComputer pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, source.RunID).Scan(&sourceComputer); err != nil {
		t.Fatal(err)
	}
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
		if locator.RunID() != pgvalue.UUID(source.RunID) || locator.AttemptNumber() != 1 || locator.EnvironmentID() != pgvalue.UUID(f.EnvironmentID) || locator.SessionID().Valid {
			t.Fatalf("locator Run=%s attempt=%d", pgvalue.UUIDString(locator.RunID()), locator.AttemptNumber())
		}
		secrets, err := locator.LockSecrets(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		execution, err := secrets.LockExecution(t.Context())
		if err != nil || execution.Run().ID != pgvalue.UUID(source.RunID) || execution.Computer().ID != sourceComputer {
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

// A binding added after the union lock changes the re-read, so Recheck
// rejects it before it could wait on the new, out-of-order Secret lock.
func TestControlSecretsRecheckRejectsNewBindingWithoutLockingIt(t *testing.T) {
	f, _, fence, targetID, targetComputer := startedSourceFixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	controls, err := LockControlSecrets(ctx, tx, fence, targetID)
	if err != nil {
		t.Fatal(err)
	}
	secretID, versionID := uuid.NewV7(), uuid.NewV7()
	committedTx(t, f, func(bind pgx.Tx) error {
		dbtest.MustExec(t, t.Context(), bind, `SET CONSTRAINTS ALL DEFERRED`)
		dbtest.MustExec(t, t.Context(), bind, `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,$3,$4)`, secretID, f.EnvironmentID, "secret-"+secretID.String(), versionID)
		dbtest.MustExec(t, t.Context(), bind, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('01',12),'hex'),decode(repeat('02',16),'hex'))`, versionID, secretID)
		dbtest.MustExec(t, t.Context(), bind, `INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, targetComputer, f.EnvironmentID, secretID)
		return nil
	})
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, blocker, `SELECT id FROM secrets WHERE id=$1 FOR UPDATE`, secretID)
	if _, _, _, err = controls.LockInterruptionLiveSource(ctx); err != nil {
		t.Fatal(err)
	}
	if err = controls.Recheck(ctx, true); !errors.Is(err, secret.ErrDeliveryUnavailable) {
		t.Fatalf("changed binding must reject without waiting for the new Secret: %v", err)
	}
	for _, binding := range controls.TargetBindings() {
		if binding.SecretID == pgvalue.UUID(secretID) {
			t.Fatal("a binding added after the union lock was reported as locked")
		}
	}
}
