package computer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// An allocated Instance with no member initializes its Computer's head; it
// has no source to restore, and an expired writer holds no preparation.
func TestInitialPreparationFencesInitializingHead(t *testing.T) {
	f := newPreparationFixture(t)
	var head pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, f.runtime).Scan(&head); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	fence, err := lockInitialFence(t.Context(), tx, f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	if fence.versionID != head || fence.writerGeneration != 1 {
		t.Fatalf("preparation=%+v", fence)
	}
	if _, err = lockSourceFence(t.Context(), tx, f.principal, f.ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unpublished source authorized: %v", err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.runtime)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = lockInitialFence(t.Context(), tx, f.principal, f.ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired writer authorized: %v", err)
	}
}

func TestComputerSourcePreparationRechecksDeadline(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_version=0,observed_desired_version=0,ready_at=NULL WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	principal := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID}
	var ref PreparationRef
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.worker_epoch,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&ref.InstanceID, &principal.Epoch, &ref.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	fence, err := lockSourceFence(t.Context(), tx, principal, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lockInitialFence(t.Context(), tx, principal, ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("committed source initialized again: %v", err)
	}
	// Time may pass during certification even though this transaction owns the locks.
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET preparation_expires_at=$2 WHERE id=$1`, pgvalue.UUID(ref.InstanceID), time.Now().Add(-time.Second))
	if err = fence.checkDeadlines(t.Context()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired preparation committed: %v", err)
	}
}
