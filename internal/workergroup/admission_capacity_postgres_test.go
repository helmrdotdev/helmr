package workergroup

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCapacityEnvelopeFencesInventoryAndObservationAge(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "capacity")
	fingerprint, err := SupplyFingerprint(t.Context(), f.q, f.groupID())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := PlanRequest{ExpectedSupplyFingerprint: fingerprint, ObservedAt: now, Pools: []PoolRequest{{PoolID: pgvalue.UUIDString(pool.ID), MaxAdditionalWorkers: 1}}}
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), request, now); err != nil {
		t.Fatal(err)
	}
	stale := request
	stale.ObservedAt = now.Add(-time.Minute)
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), stale, now); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("stale: %v", err)
	}
	future := request
	future.ObservedAt = now.Add(time.Second)
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), future, now); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("future: %v", err)
	}
	older := request
	older.ObservedAt = now.Add(-time.Second)
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), older, now); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("out of order: %v", err)
	}
	mismatch := request
	mismatch.Pools = []PoolRequest{{PoolID: pgvalue.UUIDString(pool.ID), MaxAdditionalWorkers: 2, ActiveWorkers: 1}}
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), mismatch, now); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	f.activeHost(t, pool, "new-host")
	changed, err := SupplyFingerprint(t.Context(), f.q, f.groupID())
	if err != nil || changed == fingerprint {
		t.Fatalf("topology fingerprint unchanged: %v", err)
	}
	request.ObservedAt = time.Now().UTC()
	if _, err = ObserveCapacityPlan(t.Context(), f.pool, f.groupID(), request, time.Now()); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("stale topology: %v", err)
	}
	var retained time.Time
	if err = f.pool.QueryRow(t.Context(), `SELECT capacity_observed_at FROM worker_groups WHERE id=$1`, f.groupID()).Scan(&retained); err != nil || retained.Sub(now) > time.Microsecond || now.Sub(retained) > time.Microsecond {
		t.Fatalf("rejected observation renewed envelope: %v %v", retained, err)
	}
}
