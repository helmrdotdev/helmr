package computer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// hostTransitions change the worker host after a restore was acknowledged.
// Only an epoch or status change removes the restore's worker authority.
var hostTransitions = []struct {
	name     string
	apply    func(context.Context, runtest.Fixture) error
	rejected bool
}{
	{"drain", func(ctx context.Context, f runtest.Fixture) error {
		_, err := db.New(f.Pool).DrainWorkerHost(ctx, db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1})
		return err
	}, false},
	{"host claim", execTransition(`UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`), false},
	{"group claim", execTransition(`UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`), false},
	{"new epoch", execTransition(`UPDATE worker_hosts SET current_epoch=2,claim_version=claim_version+1 WHERE id=$1`), true},
	{"lost", execTransition(`UPDATE worker_hosts SET status='lost',lost_at=now(),claim_version=claim_version+1 WHERE id=$1`), true},
}

func execTransition(sql string) func(context.Context, runtest.Fixture) error {
	return func(ctx context.Context, f runtest.Fixture) error {
		_, err := f.Pool.Exec(ctx, sql, f.WorkerID)
		return err
	}
}

// The restore fence carries no claim versions: acknowledged restores replay
// across claim-only changes and a drain, and stop at a new epoch or loss.
func TestRestoreFenceFencesEpochAndStatusNotClaims(t *testing.T) {
	for _, transition := range hostTransitions {
		t.Run(transition.name, func(t *testing.T) {
			f := newRestorePlanFixture(t, false, false)
			cp := f.commit(t)
			acknowledge := func() error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(t.Context())
				if _, err = dispatch.AcknowledgeRestore(t.Context(), tx, f.ref, cp.ID, f.writer.WriterGeneration, restoreGrants(t, f.Fixture, f.ref)); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}
			if err := acknowledge(); err != nil {
				t.Fatal(err)
			}
			if err := transition.apply(t.Context(), f.Fixture); err != nil {
				t.Fatal(err)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			restore, err := computer.LockRestore(t.Context(), tx, f.ref)
			if transition.rejected {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("restore fence after %s: %v", transition.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("restore fence after %s: %v", transition.name, err)
			}
			if restore.Instance().ID != pgvalue.UUID(f.ref.ID) || (restore.Instance().AdmissionState != "open" && restore.Instance().AdmissionState != "draining") {
				t.Fatalf("restore fence instance = %+v", restore.Instance())
			}
			if err = tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = acknowledge(); err != nil {
				t.Fatalf("acknowledgement replay after %s: %v", transition.name, err)
			}
		})
	}
}

// A first restore commit needs admitting supply; the fence reports it.
func TestRestoreFenceReportsAdmission(t *testing.T) {
	f := newRestorePlanFixture(t, false, false)
	admitting := func() bool {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		restore, err := computer.LockRestore(t.Context(), tx, f.ref)
		if err != nil {
			t.Fatal(err)
		}
		return restore.Admitting()
	}
	if !admitting() {
		t.Fatal("active supply does not admit a restore")
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE worker_groups SET status='paused',claim_version=claim_version+1 WHERE id=$1`, runtest.WorkerGroupID); err != nil {
		t.Fatal(err)
	}
	if admitting() {
		t.Fatal("paused supply admits a restore")
	}
}

func restoreGrants(t *testing.T, f runtest.Fixture, ref computer.InstanceRef) []dispatch.RestoreGrant {
	t.Helper()
	rows, err := f.Pool.Query(t.Context(), `SELECT run_id,id,lease_sequence FROM run_leases WHERE computer_instance_id=$1 ORDER BY run_id`, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dispatch.RestoreGrant, error) {
		var g dispatch.RestoreGrant
		err := row.Scan(&g.RunID, &g.LeaseID, &g.LeaseSequence)
		return g, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return grants
}
