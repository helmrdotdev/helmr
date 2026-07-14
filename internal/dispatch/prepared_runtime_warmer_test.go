package dispatch

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type fakePreparedSupply struct {
	target, limit int32
	created       []PreparedRuntimeWake
}

type fakeWakePublisher struct{ wakes []WorkerWake }

func (f *fakeWakePublisher) PublishWorkerWake(_ context.Context, wake WorkerWake) error {
	f.wakes = append(f.wakes, wake)
	return nil
}

func (f *fakePreparedSupply) ReconcilePreparedRuntimeSupply(_ context.Context, target, limit int32) ([]PreparedRuntimeWake, error) {
	f.target, f.limit = target, limit
	return f.created, nil
}

func TestRuntimePreparerPublishesNewRuntimeWake(t *testing.T) {
	workerID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runtimeID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakePreparedSupply{created: []PreparedRuntimeWake{{
		WorkerInstanceID: workerID, WorkerEpoch: 7, RuntimeInstanceID: runtimeID,
	}}}
	publisher := &fakeWakePublisher{}
	preparer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1), WithRuntimePrepareLimit(1),
		WithRuntimePrepareWakePublisher(publisher))
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.wakes) != 1 {
		t.Fatalf("published wakes = %d, want 1", len(publisher.wakes))
	}
	wake := publisher.wakes[0]
	if wake.Domain != "runtime" || wake.WorkerEpoch != 7 || wake.WorkerID != workerID || wake.RuntimeID != runtimeID || wake.AuthorityID != runtimeID {
		t.Fatalf("runtime wake = %+v", wake)
	}
}

func TestRuntimePreparerInvokesDurableSupplyReconciliation(t *testing.T) {
	store := &fakePreparedSupply{created: make([]PreparedRuntimeWake, 2)}
	preparer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(2), WithRuntimePrepareLimit(7))
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.target != 2 || store.limit != 7 {
		t.Fatalf("supply request = target %d limit %d, want 2/7", store.target, store.limit)
	}
}
