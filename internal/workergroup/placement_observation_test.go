package workergroup

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"testing"
)

func TestPlanObservesPlacementsWithoutCreatingReserveDemand(t *testing.T) {
	id := plannerTestUUID(1)
	pool := plannerTestPool(id, "primary")
	store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{pool}}
	empty, err := Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: 2}}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	p := requirePoolPlan(t, empty, id)
	if p.RecommendedAdditionalWorkers != 0 || p.Placements.AfterOptionalReclaim != 0 {
		t.Fatalf("optional reserve generated demand: %+v", p)
	}
	host := plannerBin(pool, id)
	host.ResourceObservationCurrent = true
	host.ImmediateCpuMillis = 0
	host.ImmediateMemoryBytes = 0
	host.ImmediateVmSlots = 0
	store.bins = []db.ListWorkerCapacityBinsRow{host}
	result, err := Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: 2}}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	p = requirePoolPlan(t, result, id)
	if p.RecommendedAdditionalWorkers != 0 || p.Placements.AfterOptionalReclaim != 1 || p.Placements.Immediate == nil || *p.Placements.Immediate != 0 {
		t.Fatalf("reclaimable observation: %+v", p)
	}
	store.demand = []db.ListAllocationPlanningDemandRow{plannerFreshComputer(20)}
	result, err = Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: 2}}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	p = requirePoolPlan(t, result, id)
	if p.RecommendedAdditionalWorkers != 0 {
		t.Fatalf("reclaimable work generated scale-out: %+v", p)
	}
}

func TestPlanImmediateUnknownDoesNotMeanEmpty(t *testing.T) {
	id := plannerTestUUID(1)
	pool := plannerTestPool(id, "primary")
	store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{pool}, bins: []db.ListWorkerCapacityBinsRow{plannerBin(pool, id)}}
	store.bins[0].ResourceObservationCurrent = false
	response, err := Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: 1}}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	p := requirePoolPlan(t, response, id)
	if p.Placements.Immediate != nil || p.OptionalOccupancy != nil || p.Complete || p.RecommendedAdditionalWorkers != 0 {
		t.Fatalf("unknown telemetry: %+v", p)
	}
}

func TestPlacementCountUsesCompleteShape(t *testing.T) {
	shape := ResourceVector{CPUMillis: 1000, MemoryBytes: 1024, GuestEphemeralDiskBytes: 4096, VMSlots: 1}
	for _, available := range []ResourceVector{{CPUMillis: 999, MemoryBytes: 4096, GuestEphemeralDiskBytes: 8192, VMSlots: 4}, {CPUMillis: 4000, MemoryBytes: 1000, GuestEphemeralDiskBytes: 8192, VMSlots: 4}, {CPUMillis: 4000, MemoryBytes: 4096, GuestEphemeralDiskBytes: 4095, VMSlots: 4}} {
		if n := shapeFits(available, shape, 4); n != 0 {
			t.Fatalf("partial shape counted: %+v -> %d", available, n)
		}
	}
}
