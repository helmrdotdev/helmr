package workergroup

import (
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestScaleInPostgresReplayRestartAndAbsence(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "scale")
	f.activeHost(t, pool, "first")
	f.activeHost(t, pool, "second")
	key := uuid.NewV7()
	selected, created, err := SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), pool.ID.Bytes, key)
	if err != nil || !created || selected.Phase != "draining" {
		t.Fatalf("selection: %+v %v", selected, err)
	}
	again, created, err := SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), pool.ID.Bytes, key)
	if err != nil || created || again.WorkerHostID != selected.WorkerHostID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	pending, err := PendingScaleInOperation(t.Context(), f.pool, f.groupID())
	if err != nil || pending == nil || pending.OperationKey != key.String() {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	var conflict ScaleInConflict
	if _, _, err := SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), pool.ID.Bytes, uuid.NewV7()); !errors.As(err, &conflict) || conflict.Code != "scale_in_operation_pending" {
		t.Fatalf("other key: %v", err)
	}
	if _, _, err := SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), uuid.NewV7(), key); !errors.As(err, &conflict) || conflict.Code != "scale_in_operation_key_conflict" {
		t.Fatalf("fingerprint: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, selected.WorkerHostID)
	fresh, err := GetScaleInOperation(t.Context(), f.pool, f.groupID(), key)
	if err != nil || fresh.Phase != "draining" {
		t.Fatalf("restart: %+v %v", fresh, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='termination_ready',claim_version=claim_version+1,termination_ready_at=now() WHERE id=$1`, selected.WorkerHostID)
	fresh, err = GetScaleInOperation(t.Context(), f.pool, f.groupID(), key)
	if err != nil || fresh.Phase != "termination_ready" {
		t.Fatalf("terminal: %+v %v", fresh, err)
	}
	hostID := uuid.MustParse(selected.WorkerHostID)
	if _, err := confirmSupplyAbsence(t, f, hostID); err != nil {
		t.Fatal(err)
	}
	fresh, err = GetScaleInOperation(t.Context(), f.pool, f.groupID(), key)
	if err != nil || fresh.Phase != "completed" || fresh.CompletedAt == nil {
		t.Fatalf("completion: %+v %v", fresh, err)
	}
	again, created, err = SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), pool.ID.Bytes, key)
	if err != nil || created || again.Phase != "completed" || again.WorkerHostID != selected.WorkerHostID {
		t.Fatalf("completed replay: %+v %v", again, err)
	}
}

func TestScaleInPostgresConcurrentSelectionAndFencedAbsence(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "scale")
	f.activeHost(t, pool, "first")
	f.activeHost(t, pool, "second")
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			_, _, errs[i] = SelectScaleIn(t.Context(), f.pool, audit.System("test-fixture"), f.groupID(), pool.ID.Bytes, uuid.NewV7())
		})
	}
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else {
			var conflict ScaleInConflict
			if !errors.As(err, &conflict) || conflict.Code != "scale_in_operation_pending" {
				t.Fatal(err)
			}
		}
	}
	if successes != 1 {
		t.Fatalf("concurrency: %v", errs)
	}
	pending, err := PendingScaleInOperation(t.Context(), f.pool, f.groupID())
	if err != nil || pending == nil {
		t.Fatalf("pending: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost',claim_version=claim_version+3,lost_at=now() WHERE id=$1`, pending.WorkerHostID)
	if _, err := confirmSupplyAbsence(t, f, uuid.MustParse(pending.WorkerHostID)); err != nil {
		t.Fatal(err)
	}
	pending, err = PendingScaleInOperation(t.Context(), f.pool, f.groupID())
	if err != nil || pending != nil {
		t.Fatalf("stranded receipt: %+v %v", pending, err)
	}
}
