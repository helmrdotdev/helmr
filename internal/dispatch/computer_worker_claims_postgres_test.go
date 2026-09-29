package dispatch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Capture and restore acknowledgement carry no claim versions. Their Worker
// authority is the locked host epoch and status; a claim-only change is benign.
var workerAuthorityTransitions = []struct {
	name     string
	apply    func(context.Context, runtest.Fixture) error
	rejected bool
}{
	{"drain", func(ctx context.Context, f runtest.Fixture) error {
		_, err := db.New(f.Pool).DrainWorkerHost(ctx, db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1})
		return err
	}, false},
	{"group claim", func(ctx context.Context, f runtest.Fixture) error {
		_, err := f.Pool.Exec(ctx, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, runtest.WorkerGroupID)
		return err
	}, false},
	{"new epoch", func(ctx context.Context, f runtest.Fixture) error {
		_, err := f.Pool.Exec(ctx, `UPDATE worker_hosts SET current_epoch=2,claim_version=claim_version+1 WHERE id=$1`, f.WorkerID)
		return err
	}, true},
	{"lost", func(ctx context.Context, f runtest.Fixture) error {
		_, err := f.Pool.Exec(ctx, `UPDATE worker_hosts SET status='lost',lost_at=now(),claim_version=claim_version+1 WHERE id=$1`, f.WorkerID)
		return err
	}, true},
}

func TestComputerCaptureRegistrationFencesEpochAndStatusNotClaims(t *testing.T) {
	for _, transition := range workerAuthorityTransitions {
		t.Run(transition.name, func(t *testing.T) {
			f, worker, request := dispatchtest.RegisteredCapture(t, false)
			if err := transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, request)
			if transition.rejected {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("registration after %s: %v", transition.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("registration after %s: %v", transition.name, err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestComputerRestoreAcknowledgementFencesEpochAndStatusNotClaims(t *testing.T) {
	for _, transition := range workerAuthorityTransitions {
		t.Run(transition.name, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, false)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			cp, err := authority.CommitComputerRestore(t.Context(), tx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			grants := installedRestoreGrants(t, f, fence)
			acknowledge := func() error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(t.Context())
				if _, err = dispatch.AcknowledgeComputerRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}
			if err = acknowledge(); err != nil {
				t.Fatal(err)
			}
			if err = transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			err = acknowledge()
			if transition.rejected {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("acknowledgement replay after %s: %v", transition.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("acknowledgement replay after %s: %v", transition.name, err)
			}
			var open bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT admission_state IN ('open','draining') FROM computer_instances WHERE id=$1`, fence.RuntimeID).Scan(&open); err != nil || !open {
				t.Fatalf("acknowledged admission=%v err=%v", open, err)
			}
		})
	}
}
