package workergroup

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	plannerTestVMPlatformID    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	plannerTestCPUConfigDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	plannerTestGroupID = uuid.MustParse("01900000-0000-7000-8000-000000000301")
	plannerTestNow     = time.Unix(100, 0).UTC()
)

func TestPlanFreshComputerUsesExactPrimaryPool(t *testing.T) {
	secondaryID := plannerTestUUID(1)
	primaryID := plannerTestUUID(2)
	secondary := plannerTestPool(secondaryID, "secondary")
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{secondary, primary},
		demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(11)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(secondaryID, 1),
		plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	primaryPlan := requirePoolPlan(t, plan, primaryID)
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if primaryPlan.RecommendedAdditionalWorkers != 1 || primaryPlan.CompatibleQueuedItems != 1 {
		t.Fatalf("primary pool plan = %+v", primaryPlan)
	}
	if secondaryPlan.RecommendedAdditionalWorkers != 0 || secondaryPlan.CompatibleQueuedItems != 0 {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if !plan.Complete || len(plan.UnmatchedDemand) != 0 {
		t.Fatalf("plan = %+v", plan)
	}

}

func TestPlanComputerScalesExactPrimaryPoolFromZero(t *testing.T) {
	secondaryID := plannerTestUUID(41)
	primaryID := plannerTestUUID(42)
	secondary := plannerTestPool(secondaryID, "secondary")
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{secondary, primary},
		demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(43)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(secondaryID, 1), plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	primaryPlan := requirePoolPlan(t, plan, primaryID)
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if primaryPlan.RecommendedAdditionalWorkers != 1 || primaryPlan.CompatibleQueuedItems != 1 {
		t.Fatalf("primary pool plan = %+v", primaryPlan)
	}
	if secondaryPlan.RecommendedAdditionalWorkers != 0 || secondaryPlan.CompatibleQueuedItems != 0 {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if !plan.Complete || len(plan.UnmatchedDemand) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanComputerAccountedSupplyBlocksExactRequestedPools(t *testing.T) {
	primaryID := plannerTestUUID(58)
	secondaryID := plannerTestUUID(59)
	outsideID := plannerTestUUID(60)
	primary := plannerTestPool(primaryID, "primary")
	secondary := plannerTestPool(secondaryID, "secondary")
	charged := []pgtype.UUID{secondaryID, primaryID, outsideID, primaryID}
	store := plannerStore{
		group:   plannerTestGroup(primaryID),
		pools:   []db.ListCapacityWorkerPoolsRow{primary, secondary},
		charged: charged,
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 1), plannerPoolRequest(secondaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []pgtype.UUID{primaryID, secondaryID} {
		poolPlan := requirePoolPlan(t, plan, id)
		if !poolPlan.ScaleInBlocked || poolPlan.CompatibleQueuedItems != 0 || poolPlan.RecommendedAdditionalWorkers != 0 {
			t.Fatalf("accounted pool %s plan = %+v", plannerUUIDString(id), poolPlan)
		}
	}
	if !plan.Complete || len(plan.UnmatchedDemand) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanComputerAccountedSupplyDoesNotHideOrdinaryCandidate(t *testing.T) {
	primaryID := plannerTestUUID(62)
	primary := plannerTestPool(primaryID, "primary")
	charged := []pgtype.UUID{primaryID}
	store := plannerStore{
		group:   plannerTestGroup(primaryID),
		pools:   []db.ListCapacityWorkerPoolsRow{primary},
		charged: charged, demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(64)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	poolPlan := requirePoolPlan(t, plan, primaryID)
	if !poolPlan.ScaleInBlocked || poolPlan.CompatibleQueuedItems != 1 || poolPlan.RecommendedAdditionalWorkers != 1 {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
}

func TestPlanComputerUsesExistingCompatibleBin(t *testing.T) {
	primaryID := plannerTestUUID(44)
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{primary},
		bins:   []db.ListWorkerCapacityBinsRow{plannerBin(primary, primaryID)},
		demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(45)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 0 || poolPlan.CompatibleQueuedItems != 1 {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
}

func TestPlanComputerCannotUseSecondaryOnlyRequest(t *testing.T) {
	primaryID := plannerTestUUID(53)
	secondaryID := plannerTestUUID(54)
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{
			plannerTestPool(primaryID, "primary"),
			plannerTestPool(secondaryID, "secondary"),
		},
		demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(55)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(secondaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if secondaryPlan.RecommendedAdditionalWorkers != 0 || secondaryPlan.CompatibleQueuedItems != 0 {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (Incompatibility{
		Reason: reasonProviderPool,
		Count:  1,
	}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanComputerSharesFreshRunBinConstraints(t *testing.T) {
	primaryID := plannerTestUUID(56)
	primary := plannerTestPool(primaryID, "primary")
	base := plannerBin(primary, primaryID)
	tests := []struct {
		name   string
		mutate func(*db.ListWorkerCapacityBinsRow)
	}{
		{name: "run paused", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.RunPausedReason = pgtype.Text{String: "operator", Valid: true}
		}},
		{name: "instance paused", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.VMPausedReason = pgtype.Text{String: "operator", Valid: true}
		}},
		{name: "instance start", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableInstanceStarts = 0
		}},
		{name: "VM slot", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableVMSlots = 0
		}},
		{name: "CPU", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableCPUMillis = plannerRunResources().CPUMillis - 1
		}},
		{name: "memory", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableMemoryBytes = plannerRunResources().MemoryBytes - 1
		}},
		{name: "guest disk", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableGuestEphemeralDiskBytes = plannerRunResources().GuestEphemeralDiskBytes - 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			constrained := base
			test.mutate(&constrained)
			store := plannerStore{
				group:  plannerTestGroup(primaryID),
				pools:  []db.ListCapacityWorkerPoolsRow{primary},
				bins:   []db.ListWorkerCapacityBinsRow{constrained},
				demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(57)},
			}

			plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
				plannerPoolRequest(primaryID, 1),
			}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			poolPlan := requirePoolPlan(t, plan, primaryID)
			if poolPlan.RecommendedAdditionalWorkers != 1 || poolPlan.CompatibleQueuedItems != 1 {
				t.Fatalf("pool plan = %+v, want Computer Command rejected by constrained bin and packed on a fresh Worker", poolPlan)
			}
		})
	}
}

func TestCurrentBinsBoundsWorkerAggregationInput(t *testing.T) {
	rows := make([]db.ListWorkerCapacityBinsRow, maximumPlanningWorkers+1)
	for index := range rows {
		rows[index].WorkerGroupID = pgvalue.UUID(plannerTestGroupID)
	}
	store := &countingPlannerStore{plannerStore: plannerStore{
		bins: rows,
	}}
	bins, complete, err := currentBins(context.Background(), store, plannerTestGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if complete || len(bins) != maximumPlanningWorkers {
		t.Fatalf("current bins = %d, complete = %v", len(bins), complete)
	}
	if store.workerRowLimit != maximumPlanningWorkers+1 {
		t.Fatalf("worker row limit = %d, want %d", store.workerRowLimit, maximumPlanningWorkers+1)
	}
}

func TestPlanWorkerBoundarySuppressesScaleRecommendationOnOverflow(t *testing.T) {
	for _, test := range []struct {
		name         string
		workerCount  int
		wantComplete bool
		wantScale    int32
	}{
		{name: "exact limit", workerCount: maximumPlanningWorkers, wantComplete: true, wantScale: 1},
		{name: "overflow", workerCount: maximumPlanningWorkers + 1, wantComplete: false, wantScale: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			primaryID := plannerTestUUID(70)
			primary := plannerTestPool(primaryID, "primary")
			full := plannerBin(primary, primaryID)
			full.AvailableCPUMillis = 0
			full.AvailableMemoryBytes = 0
			full.AvailableGuestEphemeralDiskBytes = 0
			full.AvailableVMSlots = 0
			full.AvailableInstanceStarts = 0
			store := plannerStore{
				group: plannerTestGroup(primaryID), pools: []db.ListCapacityWorkerPoolsRow{primary},
				bins:   make([]db.ListWorkerCapacityBinsRow, test.workerCount),
				demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(71)},
			}
			for index := range store.bins {
				store.bins[index] = full
			}

			plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
				plannerPoolRequest(primaryID, 1),
			}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			poolPlan := requirePoolPlan(t, plan, primaryID)
			if plan.Complete != test.wantComplete || poolPlan.Complete != test.wantComplete ||
				poolPlan.RecommendedAdditionalWorkers != test.wantScale {
				t.Fatalf("plan complete = %v, pool = %+v", plan.Complete, poolPlan)
			}
		})
	}
}

func TestPlanRestoreUsesCompatibleSecondaryPool(t *testing.T) {
	primaryID := plannerTestUUID(1)
	secondaryID := plannerTestUUID(2)
	primary := plannerTestPool(primaryID, "primary")
	primary.CPUShapeConfigDigests = []string{plannerDigest('c')}
	secondary := plannerTestPool(secondaryID, "secondary")
	requirements := plannerRestoreRequirements()
	if CanRestore(requirements, plannerPool(primary)) {
		t.Fatal("primary pool unexpectedly accepts the checkpoint")
	}
	if !CanRestore(requirements, plannerPool(secondary)) {
		t.Fatal("secondary pool must accept the checkpoint")
	}
	missingGroup := requirements
	missingGroup.WorkerGroupID = uuid.Nil()
	if CanRestore(missingGroup, plannerPool(secondary)) {
		t.Fatal("restore requirements without a Worker Group must be rejected")
	}
	missingGroupPool := plannerPool(secondary)
	missingGroupPool.WorkerGroupID = uuid.Nil()
	if CanRestore(requirements, missingGroupPool) {
		t.Fatal("restore pool without a Worker Group must be rejected")
	}
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{primary, secondary},
		demand: []db.ListAllocationPlanningDemandRow{plannerRestoreComputer(13, requirements)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 1),
		plannerPoolRequest(secondaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	primaryPlan := requirePoolPlan(t, plan, primaryID)
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if primaryPlan.CompatibleQueuedItems != 0 || primaryPlan.RecommendedAdditionalWorkers != 0 {
		t.Fatalf("primary pool plan = %+v", primaryPlan)
	}
	if secondaryPlan.CompatibleQueuedItems != 1 || secondaryPlan.RecommendedAdditionalWorkers != 1 || !secondaryPlan.ScaleInBlocked {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if len(plan.UnmatchedDemand) != 0 {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanRestoreScalesBoundSecondaryWhenCompatiblePrimaryIsUnboundAndFull(t *testing.T) {
	primaryID := plannerTestUUID(1)
	secondaryID := plannerTestUUID(2)
	primary := plannerTestPool(primaryID, "unbound-primary")
	secondary := plannerTestPool(secondaryID, "bound-secondary")
	fullPrimary := plannerBin(primary, primaryID)
	fullPrimary.AvailableCPUMillis = 0
	fullPrimary.AvailableMemoryBytes = 0
	fullPrimary.AvailableGuestEphemeralDiskBytes = 0
	fullPrimary.AvailableVMSlots = 0
	fullPrimary.AvailableInstanceStarts = 0
	requirements := plannerRestoreRequirements()
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{primary, secondary},
		bins:   []db.ListWorkerCapacityBinsRow{fullPrimary},
		demand: []db.ListAllocationPlanningDemandRow{plannerRestoreComputer(14, requirements)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(secondaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Pools) != 1 {
		t.Fatalf("pool plans = %+v", plan.Pools)
	}
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if secondaryPlan.RecommendedAdditionalWorkers != 1 || secondaryPlan.CompatibleQueuedItems != 1 || !secondaryPlan.ScaleInBlocked {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if len(plan.UnmatchedDemand) != 0 {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanRestorePrefersPrimaryForNewWorker(t *testing.T) {
	firstID := plannerTestUUID(1)
	secondID := plannerTestUUID(2)
	first := plannerTestPool(firstID, "first")
	first.ActiveWorkers = 2
	second := plannerTestPool(secondID, "second")
	second.ActiveWorkers = 3
	requirements := plannerRestoreRequirements()
	run := plannerRestoreComputer(15, requirements)
	group := plannerTestGroup(secondID)

	forward, err := Plan(context.Background(), plannerStore{
		group: group, pools: []db.ListCapacityWorkerPoolsRow{first, second}, demand: []db.ListAllocationPlanningDemandRow{run},
	}, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(firstID, 1), plannerPoolRequest(secondID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := Plan(context.Background(), plannerStore{
		group: group, pools: []db.ListCapacityWorkerPoolsRow{second, first}, demand: []db.ListAllocationPlanningDemandRow{run},
	}, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(secondID, 1), plannerPoolRequest(firstID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("plan depends on provider or database ordering:\nforward = %+v\nreverse = %+v", forward, reverse)
	}
	firstPlan := requirePoolPlan(t, forward, firstID)
	secondPlan := requirePoolPlan(t, forward, secondID)
	if firstPlan.CompatibleQueuedItems != 0 || firstPlan.RecommendedAdditionalWorkers != 0 || firstPlan.ScaleInBlocked {
		t.Fatalf("first pool plan = %+v", firstPlan)
	}
	if secondPlan.CompatibleQueuedItems != 1 || secondPlan.RecommendedAdditionalWorkers != 1 || !secondPlan.ScaleInBlocked {
		t.Fatalf("second pool plan = %+v", secondPlan)
	}
	if got := firstPlan.CompatibleQueuedItems + secondPlan.CompatibleQueuedItems; got != 1 {
		t.Fatalf("compatible item count = %d, want exactly 1", got)
	}
}

func TestPlanRestorePrimaryFallback(t *testing.T) {
	for _, reason := range []string{"incompatible", "unbound", "absent", "zero budget", "full"} {
		t.Run(reason, func(t *testing.T) {
			secondaryID, primaryID := plannerTestUUID(1), plannerTestUUID(2)
			secondary := plannerTestPool(secondaryID, "secondary")
			primary := plannerTestPool(primaryID, "primary")
			request := PlanRequest{Pools: []PoolRequest{plannerPoolRequest(secondaryID, 1), plannerPoolRequest(primaryID, 1)}}
			store := plannerStore{group: plannerTestGroup(primaryID)}
			store.demand = []db.ListAllocationPlanningDemandRow{plannerRestoreComputer(20, plannerRestoreRequirements())}
			switch reason {
			case "incompatible":
				primary.CPUShapeConfigDigests = []string{plannerDigest('c')}
			case "unbound":
				request.Pools = request.Pools[:1]
			case "absent":
				store.group.PrimaryPoolID = pgtype.UUID{}
			case "zero budget":
				request.Pools[1].MaxAdditionalWorkers = 0
			case "full":
				store.demand = append(store.demand, plannerFreshComputer(10))
			}
			store.pools = []db.ListCapacityWorkerPoolsRow{secondary, primary}
			plan, err := Plan(context.Background(), store, plannerTestGroupID, request, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			got := requirePoolPlan(t, plan, secondaryID)
			if got.RecommendedAdditionalWorkers != 1 || got.CompatibleQueuedItems != 1 || !got.ScaleInBlocked || !plan.Complete || len(plan.UnmatchedDemand) != 0 {
				t.Fatalf("plan = %+v", plan)
			}
			if reason == "full" {
				got = requirePoolPlan(t, plan, primaryID)
				if got.RecommendedAdditionalWorkers != 1 || got.CompatibleQueuedItems != 1 {
					t.Fatalf("primary plan = %+v", got)
				}
			}
		})
	}
}

func TestPlanRestoreReusesSecondaryCapacityBeforeNewPrimary(t *testing.T) {
	for _, physical := range []bool{true, false} {
		t.Run(fmt.Sprintf("physical=%t", physical), func(t *testing.T) {
			secondaryID, primaryID := plannerTestUUID(1), plannerTestUUID(2)
			secondary := plannerTestPool(secondaryID, "secondary")
			primary := plannerTestPool(primaryID, "primary")
			store := plannerStore{group: plannerTestGroup(primaryID)}
			store.demand = []db.ListAllocationPlanningDemandRow{plannerRestoreComputer(20, plannerRestoreRequirements())}
			wantWorkers, wantItems := int32(0), int64(1)
			if physical {
				store.bins = []db.ListWorkerCapacityBinsRow{plannerBin(secondary, primaryID)}
			} else {
				// A larger restore needs the secondary. The smaller restore must
				// reuse the resulting planned bin rather than start a primary.
				secondary.CapacityCPUMillis.Int64 = 3000
				secondary.PerVMCPUMillis.Int64 = 2000
				secondary.MaxVMSlots.Int32 = 2
				secondary.CPUShapeVCPUCounts = []int32{1, 2}
				secondary.CPUShapeConfigDigests = []string{plannerTestCPUConfigDigest, plannerTestCPUConfigDigest}
				large := plannerRestoreRequirements()
				large.Resources.CPUMillis = 2000
				large.VCPUCount = 2
				store.demand = append(store.demand, plannerRestoreComputer(21, large))
				wantWorkers, wantItems = 1, 2
			}
			store.pools = []db.ListCapacityWorkerPoolsRow{secondary, primary}
			plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
				plannerPoolRequest(secondaryID, 1), plannerPoolRequest(primaryID, 1),
			}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			got := requirePoolPlan(t, plan, secondaryID)
			if got.RecommendedAdditionalWorkers != wantWorkers || got.CompatibleQueuedItems != wantItems || !plan.Complete || len(plan.UnmatchedDemand) != 0 {
				t.Fatalf("plan = %+v", plan)
			}
			got = requirePoolPlan(t, plan, primaryID)
			if got.RecommendedAdditionalWorkers != 0 || got.CompatibleQueuedItems != 0 {
				t.Fatalf("primary plan = %+v", got)
			}
		})
	}
}

func TestPlanReportsPerPoolSaturationAndUnmatchedDemand(t *testing.T) {
	primaryID := plannerTestUUID(1)
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{plannerTestPool(primaryID, "primary")},
		demand: []db.ListAllocationPlanningDemandRow{
			plannerFreshComputer(21), plannerFreshComputer(22), plannerFreshComputer(23),
		},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 1 || poolPlan.CompatibleQueuedItems != 1 || !poolPlan.Saturated {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (Incompatibility{Reason: reasonProviderSaturated, Count: 2}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanAcceptsZeroAdditionalWorkerBudget(t *testing.T) {
	primaryID := plannerTestUUID(1)
	store := plannerStore{
		group:  plannerTestGroup(primaryID),
		pools:  []db.ListCapacityWorkerPoolsRow{plannerTestPool(primaryID, "primary")},
		demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(24)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{
		plannerPoolRequest(primaryID, 0),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 0 || poolPlan.CompatibleQueuedItems != 0 || !poolPlan.Saturated {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (Incompatibility{Reason: reasonProviderSaturated, Count: 1}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func plannerTestGroup(primaryRunPoolID pgtype.UUID) db.WorkerGroup {
	return db.WorkerGroup{
		ID: pgvalue.UUID(plannerTestGroupID), Name: "default", RegionID: "us-east-1", Status: string(WorkerGroupStatusActive),
		PrimaryPoolID: primaryRunPoolID,
	}
}

func plannerTestPool(id pgtype.UUID, name string) db.ListCapacityWorkerPoolsRow {
	return db.ListCapacityWorkerPoolsRow{
		ID: id, WorkerGroupID: pgvalue.UUID(plannerTestGroupID), Name: name,
		VMPlatformID:                    pgtype.Text{String: plannerTestVMPlatformID, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: 1000, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: 2 << 30, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: 64 << 30, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: 1000, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: 2 << 30, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: 64 << 30, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: 1, Valid: true},
		CPUShapeVCPUCounts:              []int32{1},
		CPUShapeConfigDigests:           []string{plannerTestCPUConfigDigest},
	}
}

func plannerPool(row db.ListCapacityWorkerPoolsRow) Pool {
	shapes := make([]vmplatform.CPUShape, len(row.CPUShapeVCPUCounts))
	for index := range row.CPUShapeVCPUCounts {
		shapes[index] = vmplatform.CPUShape{
			VCPUCount: row.CPUShapeVCPUCounts[index], CPUConfigDigest: row.CPUShapeConfigDigests[index],
		}
	}
	return Pool{
		WorkerGroupID: pgvalue.MustUUIDValue(row.WorkerGroupID), VMPlatformID: row.VMPlatformID.String,
		PerVM: ResourceVector{
			CPUMillis: row.PerVMCPUMillis.Int64, MemoryBytes: row.PerVMMemoryBytes.Int64,
			GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes.Int64,
		},
		CPUShapes: shapes,
	}
}

func plannerBin(row db.ListCapacityWorkerPoolsRow, primaryRunPoolID pgtype.UUID) db.ListWorkerCapacityBinsRow {
	return db.ListWorkerCapacityBinsRow{
		WorkerGroupID: pgvalue.UUID(plannerTestGroupID), PrimaryPoolID: primaryRunPoolID, WorkerPoolID: row.ID,
		WorkerHostID: plannerTestUUID(row.ID.Bytes[15] + 100),
		VMPlatformID: row.VMPlatformID, Arch: "x86_64", Contract: vmplatform.Contract,
		PerVMCPUMillis: row.PerVMCPUMillis.Int64, PerVMMemoryBytes: row.PerVMMemoryBytes.Int64,
		PerVMGuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes.Int64,
		AvailableCPUMillis:           row.CapacityCPUMillis.Int64, AvailableMemoryBytes: row.CapacityMemoryBytes.Int64,
		AvailableGuestEphemeralDiskBytes: row.CapacityGuestEphemeralDiskBytes.Int64,
		AvailableVMSlots:                 int64(row.MaxVMSlots.Int32),
		AvailableInstanceStarts:          int64(row.MaxVMSlots.Int32),
		CPUShapeVCPUCounts:               append([]int32(nil), row.CPUShapeVCPUCounts...),
		CPUShapeConfigDigests:            append([]string(nil), row.CPUShapeConfigDigests...),
	}
}

func plannerFreshComputer(seed byte) db.ListAllocationPlanningDemandRow {
	return db.ListAllocationPlanningDemandRow{RemainingCpuMillis: 100000000, RemainingMemoryBytes: 100000000000000, RemainingResidents: 100000, EnvironmentID: plannerTestUUID(1), OwnerID: plannerTestUUID(seed), Kind: "computer", Resources: []byte(`{"milliCpu":1000,"memoryMiB":1024}`)}
}
func plannerRestoreComputer(seed byte, requirements RestoreRequirements) db.ListAllocationPlanningDemandRow {
	row := plannerFreshComputer(seed)
	row.RequiredWorkerGroupID = pgvalue.UUID(requirements.WorkerGroupID)
	row.RequiredVMPlatformID = requirements.VMPlatformID
	row.RequiredVMVCPUCount = requirements.VCPUCount
	row.RequiredCPUConfigDigest = requirements.CPUConfigDigest
	row.RequiredCPUMillis = requirements.Resources.CPUMillis
	row.RequiredMemoryBytes = requirements.Resources.MemoryBytes
	row.RequiredScratchBytes = requirements.Resources.GuestEphemeralDiskBytes
	return row
}

func plannerRestoreRequirements() RestoreRequirements {
	return RestoreRequirements{
		WorkerGroupID: plannerTestGroupID, VMPlatformID: plannerTestVMPlatformID,
		VCPUCount: 1, CPUConfigDigest: plannerTestCPUConfigDigest,
		Resources: plannerRunResources(),
	}
}

func plannerRunResources() ResourceVector {
	return ResourceVector{
		CPUMillis: 1000, MemoryBytes: 1 << 30, GuestEphemeralDiskBytes: 32 << 30, VMSlots: 1,
	}
}

func plannerPoolRequest(id pgtype.UUID, max int32) PoolRequest {
	return PoolRequest{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: max}
}

func requirePoolPlan(t *testing.T, plan PlanResponse, id pgtype.UUID) PoolPlan {
	t.Helper()
	want := plannerUUIDString(id)
	for _, pool := range plan.Pools {
		if pool.PoolID == want {
			return pool
		}
	}
	t.Fatalf("pool %s is missing from %+v", want, plan.Pools)
	return PoolPlan{}
}

func plannerTestUUID(seed byte) pgtype.UUID {
	value := uuid.MustParse(fmt.Sprintf("0192f3a4-b5c6-7000-8000-%012x", seed))
	return pgtype.UUID{Bytes: [16]byte(value), Valid: true}
}

func plannerUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func plannerDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}

type plannerStore struct {
	charged []pgtype.UUID
	group   db.WorkerGroup
	pools   []db.ListCapacityWorkerPoolsRow
	bins    []db.ListWorkerCapacityBinsRow
	demand  []db.ListAllocationPlanningDemandRow
}
type countingPlannerStore struct {
	plannerStore
	workerRowLimit int32
	demandRowLimit int32
}

func (s *countingPlannerStore) ListAllocationPlanningDemand(ctx context.Context, arg db.ListAllocationPlanningDemandParams) ([]db.ListAllocationPlanningDemandRow, error) {
	s.demandRowLimit = arg.RowLimit
	return s.plannerStore.ListAllocationPlanningDemand(ctx, arg)
}

func (s *countingPlannerStore) ListWorkerCapacityBins(
	ctx context.Context,
	arg db.ListWorkerCapacityBinsParams,
) ([]db.ListWorkerCapacityBinsRow, error) {
	s.workerRowLimit = arg.RowLimit
	return s.plannerStore.ListWorkerCapacityBins(ctx, arg)
}

func (s plannerStore) ListAllocationPlanningChargedPools(context.Context, pgtype.UUID) ([]pgtype.UUID, error) {
	return s.charged, nil
}

func (s plannerStore) GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s plannerStore) ListCapacityWorkerPools(_ context.Context, arg db.ListCapacityWorkerPoolsParams) ([]db.ListCapacityWorkerPoolsRow, error) {
	requested := make(map[[16]byte]struct{}, len(arg.WorkerPoolIDs))
	for _, id := range arg.WorkerPoolIDs {
		requested[id.Bytes] = struct{}{}
	}
	result := make([]db.ListCapacityWorkerPoolsRow, 0, len(s.pools))
	for _, row := range s.pools {
		if row.WorkerGroupID != arg.WorkerGroupID {
			continue
		}
		if _, ok := requested[row.ID.Bytes]; ok {
			result = append(result, row)
		}
	}
	return result, nil
}

func (s plannerStore) ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error) {
	return s.bins, nil
}

func (s plannerStore) ListAllocationPlanningDemand(_ context.Context, arg db.ListAllocationPlanningDemandParams) ([]db.ListAllocationPlanningDemandRow, error) {
	return s.demand[:min(len(s.demand), int(arg.RowLimit))], nil
}

func TestPlanExistingComputerAllocationDoesNotDemandAnotherVM(t *testing.T) {
	primaryID := plannerTestUUID(94)
	charged := []pgtype.UUID{primaryID}
	store := plannerStore{group: plannerTestGroup(primaryID), pools: []db.ListCapacityWorkerPoolsRow{plannerTestPool(primaryID, "primary")},
		charged: charged}
	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{plannerPoolRequest(primaryID, 1)}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	pool := requirePoolPlan(t, plan, primaryID)
	if pool.CompatibleQueuedItems != 0 || pool.RecommendedAdditionalWorkers != 0 || !pool.ScaleInBlocked {
		t.Fatalf("existing Instance demand: %+v", pool)
	}
}

func TestDiscoverItemsBoundsUnifiedDemand(t *testing.T) {
	for _, size := range []int{int(maximumPlanningCandidates), int(maximumPlanningCandidates) + 1} {
		rows := make([]db.ListAllocationPlanningDemandRow, size)
		for i := range rows {
			rows[i] = plannerFreshComputer(byte(i))
		}
		store := &countingPlannerStore{plannerStore: plannerStore{demand: rows}}
		items, _, complete, err := discoverItems(context.Background(), store, plannerTestGroup(plannerTestUUID(2)))
		if err != nil || len(items) != int(maximumPlanningCandidates) || complete != (size == int(maximumPlanningCandidates)) || store.demandRowLimit != maximumPlanningCandidates+1 {
			t.Fatalf("size=%d items=%d complete=%v err=%v", size, len(items), complete, err)
		}
	}
}
func TestPlanFreshComputerRequiresQualifiedCPUShape(t *testing.T) {
	id := plannerTestUUID(2)
	pool := plannerTestPool(id, "primary")
	pool.CPUShapeVCPUCounts = []int32{2}
	store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{pool}, demand: []db.ListAllocationPlanningDemandRow{plannerFreshComputer(3)}}
	plan, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{plannerPoolRequest(id, 1)}}, plannerTestNow)
	if err != nil || requirePoolPlan(t, plan, id).RecommendedAdditionalWorkers != 0 || len(plan.UnmatchedDemand) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestPlanRespectsAggregateEnvironmentCapacity(t *testing.T) {
	for _, resource := range []string{"cpu", "memory", "resident"} {
		t.Run(resource, func(t *testing.T) {
			id := plannerTestUUID(2)
			first, second := plannerFreshComputer(3), plannerFreshComputer(4)
			for _, row := range []*db.ListAllocationPlanningDemandRow{&first, &second} {
				switch resource {
				case "cpu":
					row.RemainingCpuMillis = 1000
				case "memory":
					row.RemainingMemoryBytes = 1024 * mebibyte
				case "resident":
					row.RemainingResidents = 1
				}
			}
			store := plannerStore{group: plannerTestGroup(id), pools: []db.ListCapacityWorkerPoolsRow{plannerTestPool(id, "primary")}, demand: []db.ListAllocationPlanningDemandRow{first, second}}
			result, err := Plan(context.Background(), store, plannerTestGroupID, PlanRequest{Pools: []PoolRequest{plannerPoolRequest(id, 2)}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			pool := requirePoolPlan(t, result, id)
			if pool.CompatibleQueuedItems != 1 || pool.RecommendedAdditionalWorkers != 1 {
				t.Fatalf("overcommitted environment: %+v", result)
			}
		})
	}
}
