package workergroup

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPlanSpareSlots(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		ready, demand, headroom int
		alter                   func(*db.ListWorkerCapacityBinsRow)
		additional, shortfall   int32
		blocked, saturated      bool
	}{
		{name: "empty fleet", headroom: 2, additional: 1, shortfall: 1, blocked: true},
		{name: "one ready", ready: 1, headroom: 1, blocked: true},
		{name: "two ready can shrink", ready: 2},
		{name: "queued work consumes reserve", ready: 1, demand: 1, headroom: 1, additional: 1, shortfall: 1, blocked: true},
		{name: "queued work before reserve", demand: 1, headroom: 2, additional: 2, shortfall: 1, blocked: true},
		{name: "ceiling exhausted", ready: 1, demand: 1, shortfall: 1, blocked: true, saturated: true},
		{name: "paused is not ready", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) {
			b.AdmissionPausedReason = pgtype.Text{String: "pressure", Valid: true}
		}, additional: 1, shortfall: 1, blocked: true},
		{name: "memory exhausted", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) { b.AvailableMemoryBytes = 1 }, additional: 1, shortfall: 1, blocked: true},
		{name: "disk exhausted", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) { b.AvailableGuestEphemeralDiskBytes = 1 }, additional: 1, shortfall: 1, blocked: true},
		{name: "wrong runtime", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) { b.VMPlatformID.String = "different" }, additional: 1, shortfall: 1, blocked: true},
		{name: "missing cpu shape", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) { b.CPUShapeVCPUCounts = nil; b.CPUShapeConfigDigests = nil }, additional: 1, shortfall: 1, blocked: true},
		{name: "starts occupied", ready: 1, headroom: 1, alter: func(b *db.ListWorkerCapacityBinsRow) { b.AvailableInstanceStarts = 0 }, additional: 1, shortfall: 1, blocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := plannerTestUUID(1)
			pool := plannerTestPool(id, "primary")
			store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{pool}}
			for i := 0; i < tt.ready; i++ {
				b := plannerBin(pool, id)
				if tt.alter != nil {
					tt.alter(&b)
				}
				store.bins = append(store.bins, b)
			}
			for i := 0; i < tt.demand; i++ {
				store.demand = append(store.demand, plannerFreshComputer(byte(i+20)))
			}
			response, err := Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: int32(tt.headroom), MinSpareSlots: 1}}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			p := requirePoolPlan(t, response, id)
			if p.RecommendedAdditionalWorkers != tt.additional || p.SpareSlotsShortfall != tt.shortfall || p.ScaleInBlocked != tt.blocked || p.Saturated != tt.saturated {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestPlanSpareSlotsPacksFullShapes(t *testing.T) {
	for _, ready := range []bool{false, true} {
		id := plannerTestUUID(1)
		pool := plannerTestPool(id, "primary")
		pool.CapacityCPUMillis.Int64 *= 2
		pool.CapacityMemoryBytes.Int64 *= 2
		pool.CapacityGuestEphemeralDiskBytes.Int64 *= 2
		pool.MaxVMSlots.Int32 = 2
		store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{pool}, demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(20)}}
		if ready {
			store.bins = []db.ListWorkerCapacityBinsRow{plannerBin(pool, id)}
		}
		response, err := Plan(t.Context(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: 1, MinSpareSlots: 1}}}, plannerTestNow)
		if err != nil {
			t.Fatal(err)
		}
		p := requirePoolPlan(t, response, id)
		var expected int32 = 1
		if ready {
			expected = 0
		}
		if p.RecommendedAdditionalWorkers != expected || p.SpareSlotsShortfall != expected || p.Saturated {
			t.Fatalf("ready=%v plan=%+v", ready, p)
		}
	}
}

func TestPlanRejectsInvalidSpareSlots(t *testing.T) {
	for _, minimum := range []int32{-1, 1001} {
		id := plannerTestUUID(1)
		_, err := Plan(t.Context(), plannerStore{}, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{{PoolID: plannerUUIDString(id), MinSpareSlots: minimum}}}, plannerTestNow)
		if err == nil {
			t.Fatalf("accepted reserve %d", minimum)
		}
	}
}
