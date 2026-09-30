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

// Capture carries no claim versions. Its Worker authority is the locked host
// epoch and status; a claim-only change is benign.
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
