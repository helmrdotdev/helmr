package fleet

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

func TestPlanZeroWorkersWithQueuedDemand(t *testing.T) {
	planner := mustPlanner(t, testPolicy())

	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{
			Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 1_000, MemoryBytes: 2_000, ScratchBytes: 200, VMSlots: 1}, 5)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionLaunch || decision.LaunchCount != 3 {
		t.Fatalf("decision = %#v, want launch 3", decision)
	}
	if decision.DesiredWorkers != 3 {
		t.Fatalf("desired workers = %d, want 3", decision.DesiredWorkers)
	}
}

func TestPlanZeroDemandHonorsWarmFloor(t *testing.T) {
	policy := testPolicy()
	policy.MinWorkers = 1
	policy.WarmWorkers = 2
	planner := mustPlanner(t, policy)

	decision, err := planner.Plan(Inputs{Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionLaunch || decision.LaunchCount != 2 || decision.DesiredWorkers != 2 {
		t.Fatalf("decision = %#v, want warm-floor launch of 2", decision)
	}
}

func TestRunAttestationCoverageGapRaisesOneWorkerFloor(t *testing.T) {
	planner := mustPlanner(t, testPolicy())
	decision, err := planner.Plan(Inputs{Now: testNow, UncertifiedRunLaunchAttestations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionLaunch || decision.LaunchCount != 1 || decision.DesiredWorkers != 1 || decision.UncappedRequiredWorkers != 1 {
		t.Fatalf("decision = %#v, want one certification worker", decision)
	}
}

func TestRunAttestationCoverageGapDoesNotAddToWarmFloor(t *testing.T) {
	policy := testPolicy()
	policy.WarmWorkers = 2
	planner := mustPlanner(t, policy)
	decision, err := planner.Plan(Inputs{Now: testNow, UncertifiedRunLaunchAttestations: 3})
	if err != nil {
		t.Fatal(err)
	}
	if decision.DesiredWorkers != 2 || decision.LaunchCount != 2 {
		t.Fatalf("decision = %#v, want existing warm floor of two", decision)
	}
}

func TestRunAttestationCoverageGapReusesPendingSupply(t *testing.T) {
	planner := mustPlanner(t, testPolicy())
	decision, err := planner.Plan(Inputs{
		Now: testNow, UncertifiedRunLaunchAttestations: 1,
		Workers: []Worker{{ID: "provider-pending", State: WorkerPending}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionNone || decision.DesiredWorkers != 1 || decision.PlannedWorkers != 1 {
		t.Fatalf("decision = %#v, want pending supply to prevent relaunch", decision)
	}
}

func TestRunAttestationCoverageGapRespectsLaunchGuards(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Policy)
		inputs     Inputs
		wantReason Reason
		wantCap    CapReason
	}{
		{name: "emergency stop", mutate: func(policy *Policy) { policy.EmergencyStop = true }, inputs: Inputs{Now: testNow}, wantReason: ReasonEmergencyStop},
		{name: "cooldown", mutate: func(policy *Policy) { policy.ScaleOutCooldown = time.Hour }, inputs: Inputs{Now: testNow, LastScaleOutAt: testNow.Add(-time.Minute)}, wantReason: ReasonScaleOutCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := testPolicy()
			tt.mutate(&policy)
			planner := mustPlanner(t, policy)
			tt.inputs.UncertifiedRunLaunchAttestations = 1
			decision, err := planner.Plan(tt.inputs)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != ActionNone || decision.Reason != tt.wantReason || decision.CapReason != tt.wantCap {
				t.Fatalf("decision = %#v, want reason=%q cap=%q", decision, tt.wantReason, tt.wantCap)
			}
		})
	}
}

func TestNegativeRunAttestationCoverageGapFailsClosed(t *testing.T) {
	_, err := mustPlanner(t, testPolicy()).Plan(Inputs{Now: testNow, UncertifiedRunLaunchAttestations: -1})
	if !errors.Is(err, ErrInvalidInputs) {
		t.Fatalf("error = %v, want invalid inputs", err)
	}
}

func TestPlanUsesLargestResourceDimensionCeiling(t *testing.T) {
	policy := testPolicy()
	policy.InstanceCapacity = Capacity{
		MilliCPU:       4,
		MemoryBytes:    8,
		ScratchBytes:   10,
		VMSlots:        4,
		BuildExecutors: 2,
	}
	planner := mustPlanner(t, policy)

	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{
			Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 3, MemoryBytes: 7, ScratchBytes: 6, VMSlots: 3, BuildExecutors: 2}, 6)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := DimensionCeilings{MilliCPU: 5, MemoryBytes: 6, ScratchBytes: 4, VMSlots: 5, BuildExecutors: 6}
	if decision.RequiredByDimension != want {
		t.Fatalf("dimension ceilings = %#v, want %#v", decision.RequiredByDimension, want)
	}
	if decision.DesiredWorkers != 6 {
		t.Fatalf("desired workers = %d, want build-executor ceiling 6", decision.DesiredWorkers)
	}
}

func TestDiskPartitionsAreCapacityIsolated(t *testing.T) {
	policy := testPolicy()
	policy.InstanceCapacity = Capacity{
		WorkloadDiskBytes:  100,
		ScratchBytes:       100,
		BuildCacheBytes:    100,
		ArtifactCacheBytes: 100,
	}
	planner := mustPlanner(t, policy)

	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{Queued: []WorkloadBucket{
			workload(Capacity{WorkloadDiskBytes: 60}, 2),
			workload(Capacity{ScratchBytes: 60}, 2),
			workload(Capacity{BuildCacheBytes: 60}, 2),
			workload(Capacity{ArtifactCacheBytes: 60}, 2),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := DimensionCeilings{WorkloadDiskBytes: 2, ScratchBytes: 2, BuildCacheBytes: 2, ArtifactCacheBytes: 2}
	if decision.RequiredByDimension != want || decision.DesiredWorkers != 2 {
		t.Fatalf("decision = %#v, want four isolated partition ceilings of 2", decision)
	}
}

func TestIndivisibleWorkloadsCannotBeAveragedAcrossHosts(t *testing.T) {
	policy := testPolicy()
	policy.InstanceCapacity = Capacity{MilliCPU: 10}
	planner := mustPlanner(t, policy)

	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{Queued: []WorkloadBucket{
			workload(Capacity{MilliCPU: 6}, 3),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.RequiredByDimension.MilliCPU != 2 {
		t.Fatalf("aggregate CPU lower bound = %d, want 2", decision.RequiredByDimension.MilliCPU)
	}
	if decision.ConservativePackedWorkers != 3 || decision.DesiredWorkers != 3 {
		t.Fatalf("decision = %#v, want three indivisible hosts", decision)
	}
}

func TestMixedShapesUseConservativeDeterministicPacking(t *testing.T) {
	policy := testPolicy()
	policy.InstanceCapacity = Capacity{MilliCPU: 10, MemoryBytes: 10}
	planner := mustPlanner(t, policy)
	demand := Demand{Queued: []WorkloadBucket{
		workload(Capacity{MilliCPU: 6, MemoryBytes: 1}, 2),
		workload(Capacity{MilliCPU: 1, MemoryBytes: 6}, 2),
	}}

	first, err := planner.Plan(Inputs{Now: testNow, Demand: demand})
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.Plan(Inputs{Now: testNow, Demand: Demand{Queued: []WorkloadBucket{demand.Queued[1], demand.Queued[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ConservativePackedWorkers != 2 || first.DesiredWorkers != 2 {
		t.Fatalf("decision = %#v, want complementary shapes packed into 2 hosts", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("packing changed with input order: first=%#v second=%#v", first, second)
	}
}

func TestCompatibilityPartitionsCannotShareAHost(t *testing.T) {
	policy := testPolicy()
	policy.InstanceCapacity = Capacity{MilliCPU: 10}
	policy.AllowedCompatibilityKeys = []string{"linux-amd64", "linux-arm64"}
	planner := mustPlanner(t, policy)
	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{Queued: []WorkloadBucket{
			{CompatibilityKey: "linux-amd64", Shape: Capacity{MilliCPU: 4}, Count: 1},
			{CompatibilityKey: "linux-arm64", Shape: Capacity{MilliCPU: 4}, Count: 1},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []CompatibilityRequirement{
		{CompatibilityKey: "linux-amd64", Workers: 1},
		{CompatibilityKey: "linux-arm64", Workers: 1},
	}
	if decision.RequiredByDimension.MilliCPU != 1 || decision.ConservativePackedWorkers != 2 {
		t.Fatalf("decision = %#v, want compatibility-isolated requirement 2", decision)
	}
	if !reflect.DeepEqual(decision.RequiredByCompatibility, want) {
		t.Fatalf("compatibility requirements = %#v, want %#v", decision.RequiredByCompatibility, want)
	}
}

func TestUnsupportedOrOversizedWorkloadFailsClosed(t *testing.T) {
	planner := mustPlanner(t, testPolicy())
	tests := []Demand{
		{Queued: []WorkloadBucket{{CompatibilityKey: "unsupported", Shape: Capacity{MilliCPU: 1}, Count: 1}}},
		{Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 2_001}, 1)}},
	}
	for _, demand := range tests {
		if _, err := planner.Plan(Inputs{Now: testNow, Demand: demand}); !errors.Is(err, ErrInvalidInputs) {
			t.Fatalf("Plan error = %v, want invalid inputs for %#v", err, demand)
		}
	}
}

func TestMixedShapePackingHasBoundedExpansion(t *testing.T) {
	policy := testPolicy()
	policy.MaxPackingItems = 3
	planner := mustPlanner(t, policy)
	mixed := Demand{Queued: []WorkloadBucket{
		workload(Capacity{MilliCPU: 1_000}, 2),
		workload(Capacity{MemoryBytes: 2_000}, 2),
	}}
	if _, err := planner.Plan(Inputs{Now: testNow, Demand: mixed}); !errors.Is(err, ErrPackingLimit) {
		t.Fatalf("mixed packing error = %v, want packing limit", err)
	}

	uniform := Demand{Queued: []WorkloadBucket{
		workload(Capacity{MilliCPU: 1_000}, 100),
	}}
	decision, err := planner.Plan(Inputs{Now: testNow, Demand: uniform})
	if err != nil {
		t.Fatalf("uniform bucket should use closed-form packing: %v", err)
	}
	if decision.ConservativePackedWorkers != 50 {
		t.Fatalf("uniform packed workers = %d, want 50", decision.ConservativePackedWorkers)
	}
}

func TestPendingWorkersPreventRepeatedOverLaunch(t *testing.T) {
	planner := mustPlanner(t, testPolicy())
	demand := Demand{
		Active: []WorkloadBucket{workload(Capacity{MilliCPU: 1_000, MemoryBytes: 2_000, VMSlots: 1}, 2)},
		Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 1_000, MemoryBytes: 2_000, VMSlots: 1}, 4)},
	}

	first, err := planner.Plan(Inputs{Now: testNow, Demand: demand})
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != ActionLaunch || first.LaunchCount != 3 {
		t.Fatalf("first decision = %#v, want launch 3", first)
	}

	second, err := planner.Plan(Inputs{
		Now:    testNow.Add(time.Second),
		Demand: demand,
		Workers: []Worker{
			{ID: "pending-a", State: WorkerPending},
			{ID: "pending-b", State: WorkerPending},
			{ID: "pending-c", State: WorkerPending},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != ActionNone || second.PlannedWorkers != 3 {
		t.Fatalf("second decision = %#v, want pending supply to prevent another launch", second)
	}
}

func TestHardMaxCapsDesiredAndPreservesUncappedDeficit(t *testing.T) {
	policy := testPolicy()
	policy.MaxWorkers = 3
	planner := mustPlanner(t, policy)
	decision, err := planner.Plan(Inputs{
		Now: testNow, Demand: largeDemand(), Workers: activeWorkers(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionNone || decision.Reason != ReasonMaximumSaturated || decision.CapReason != CapMaximum {
		t.Fatalf("decision = %#v, want maximum saturation", decision)
	}
	if decision.UncappedRequiredWorkers != 10 || decision.DesiredWorkers != 3 || decision.UnmetRequiredWorkers != 7 {
		t.Fatalf("decision = %#v, want uncapped=10 capped=3 unmet=7", decision)
	}
}

func TestScaleOutIsBoundedPerCycleAndByPendingCap(t *testing.T) {
	t.Run("cycle step", func(t *testing.T) {
		policy := testPolicy()
		policy.MaxScaleOutPerCycle = 2
		planner := mustPlanner(t, policy)
		decision, err := planner.Plan(Inputs{Now: testNow, Demand: largeDemand()})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ActionLaunch || decision.LaunchCount != 2 {
			t.Fatalf("decision = %#v, want step of 2", decision)
		}
	})

	t.Run("pending cap", func(t *testing.T) {
		policy := testPolicy()
		policy.MaxPendingWorkers = 3
		planner := mustPlanner(t, policy)
		decision, err := planner.Plan(Inputs{
			Now:     testNow,
			Demand:  largeDemand(),
			Workers: []Worker{{ID: "pending-a", State: WorkerPending}, {ID: "pending-b", State: WorkerPending}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ActionLaunch || decision.LaunchCount != 1 {
			t.Fatalf("decision = %#v, want one launch before pending cap", decision)
		}
	})
}

func TestScaleCooldownsAndHysteresis(t *testing.T) {
	t.Run("scale out cooldown", func(t *testing.T) {
		policy := testPolicy()
		policy.ScaleOutCooldown = time.Minute
		planner := mustPlanner(t, policy)
		decision, err := planner.Plan(Inputs{
			Now:            testNow,
			LastScaleOutAt: testNow.Add(-30 * time.Second),
			Demand:         largeDemand(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ActionNone || decision.Reason != ReasonScaleOutCooldown {
			t.Fatalf("decision = %#v, want scale-out cooldown", decision)
		}
	})

	t.Run("scale in cooldown", func(t *testing.T) {
		policy := testPolicy()
		policy.ScaleInCooldown = time.Minute
		planner := mustPlanner(t, policy)
		decision, err := planner.Plan(Inputs{
			Now:           testNow,
			LastScaleInAt: testNow.Add(-30 * time.Second),
			Workers:       activeWorkers(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ActionNone || decision.Reason != ReasonScaleInCooldown {
			t.Fatalf("decision = %#v, want scale-in cooldown", decision)
		}
	})

	t.Run("hysteresis before and after threshold", func(t *testing.T) {
		policy := testPolicy()
		policy.ScaleInHysteresis = 5 * time.Minute
		planner := mustPlanner(t, policy)
		before, err := planner.Plan(Inputs{
			Now:                testNow,
			UnderutilizedSince: testNow.Add(-4 * time.Minute),
			Workers:            activeWorkers(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		if before.Action != ActionNone || before.Reason != ReasonScaleInHysteresis {
			t.Fatalf("before = %#v, want hysteresis", before)
		}

		after, err := planner.Plan(Inputs{
			Now:                testNow,
			UnderutilizedSince: testNow.Add(-5 * time.Minute),
			Workers:            activeWorkers(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		if after.Action != ActionBeginDrain {
			t.Fatalf("after = %#v, want drain after hysteresis", after)
		}
	})
}

func TestEmergencyStopBlocksScaleOutWithoutKillingActiveWork(t *testing.T) {
	policy := testPolicy()
	policy.EmergencyStop = true
	planner := mustPlanner(t, policy)
	decision, err := planner.Plan(Inputs{
		Now: testNow,
		Demand: Demand{
			Active: []WorkloadBucket{workload(Capacity{MilliCPU: 1_000, MemoryBytes: 2_000, VMSlots: 1}, 2)},
			Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 1_000, MemoryBytes: 2_000, VMSlots: 1}, 4)},
		},
		Workers: activeWorkers(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionNone || decision.Reason != ReasonEmergencyStop {
		t.Fatalf("decision = %#v, want no new scale-out", decision)
	}
}

func TestDrainVictimUsesAuthorityThenAgeThenID(t *testing.T) {
	planner := mustPlanner(t, testPolicy())
	workers := []Worker{
		{ID: "high-authority", State: WorkerActive, AuthorityCount: 2, ActivatedAt: testNow.Add(-4 * time.Hour)},
		{ID: "new-low", State: WorkerActive, AuthorityCount: 0, ActivatedAt: testNow.Add(-time.Hour)},
		{ID: "b-old-low", State: WorkerActive, AuthorityCount: 0, ActivatedAt: testNow.Add(-2 * time.Hour)},
		{ID: "a-old-low", State: WorkerActive, AuthorityCount: 0, ActivatedAt: testNow.Add(-2 * time.Hour)},
	}
	decision, err := planner.Plan(Inputs{Now: testNow, Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionBeginDrain || decision.WorkerID != "a-old-low" {
		t.Fatalf("decision = %#v, want deterministic a-old-low drain", decision)
	}
}

func TestTerminationRequiresExactDisabledZeroAuthorityCleanVictim(t *testing.T) {
	tests := []struct {
		name       string
		candidate  Worker
		wantAction Action
		wantReason Reason
	}{
		{
			name:       "draining is not disabled",
			candidate:  Worker{ID: "victim", State: WorkerDraining, LocalCleanupComplete: true},
			wantAction: ActionNone,
			wantReason: ReasonTerminationNotReady,
		},
		{
			name:       "authority remains",
			candidate:  Worker{ID: "victim", State: WorkerDisabled, AuthorityCount: 1, LocalCleanupComplete: true},
			wantAction: ActionNone,
			wantReason: ReasonTerminationNotReady,
		},
		{
			name:       "cleanup incomplete",
			candidate:  Worker{ID: "victim", State: WorkerDisabled},
			wantAction: ActionNone,
			wantReason: ReasonTerminationNotReady,
		},
		{
			name:       "ready",
			candidate:  Worker{ID: "victim", State: WorkerDisabled, LocalCleanupComplete: true},
			wantAction: ActionFinishTermination,
			wantReason: ReasonFinishTermination,
		},
	}
	planner := mustPlanner(t, testPolicy())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := planner.Plan(Inputs{
				Now:                    testNow,
				Workers:                []Worker{tt.candidate},
				TerminationCandidateID: "victim",
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != tt.wantAction || decision.Reason != tt.wantReason {
				t.Fatalf("decision = %#v, want action %v reason %q", decision, tt.wantAction, tt.wantReason)
			}
		})
	}

	_, err := planner.Plan(Inputs{
		Now:                    testNow,
		Workers:                []Worker{{ID: "other", State: WorkerDisabled, LocalCleanupComplete: true}},
		TerminationCandidateID: "victim",
	})
	if !errors.Is(err, ErrInvalidInputs) {
		t.Fatalf("missing exact candidate error = %v, want invalid inputs", err)
	}
}

func TestRunAndBuildPlannersDoNotShareState(t *testing.T) {
	runPolicy := testPolicy()
	buildPolicy := testPolicy()
	buildPolicy.EmergencyStop = true
	runPlanner := mustPlanner(t, runPolicy)
	buildPlanner := mustPlanner(t, buildPolicy)
	inputs := Inputs{Now: testNow, Demand: largeDemand()}

	runFirst, err := runPlanner.Plan(inputs)
	if err != nil {
		t.Fatal(err)
	}
	build, err := buildPlanner.Plan(inputs)
	if err != nil {
		t.Fatal(err)
	}
	runSecond, err := runPlanner.Plan(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if runFirst.Action != ActionLaunch || !reflect.DeepEqual(runSecond, runFirst) {
		t.Fatalf("run decisions changed across build planning: first=%#v second=%#v", runFirst, runSecond)
	}
	if build.Action != ActionNone || build.Reason != ReasonEmergencyStop {
		t.Fatalf("build decision = %#v, want independent emergency stop", build)
	}
}

func TestFixtureCostArithmetic(t *testing.T) {
	cost, err := CostForWorkers(200, 125_000)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 25_000_000 {
		t.Fatalf("cost = %d micro-USD, want 25000000", cost)
	}
	if _, err := CostForWorkers(1, 0); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("zero price error = %v, want invalid policy", err)
	}
	if _, err := CostForWorkers(math.MaxInt, MicroUSD(math.MaxUint64)); !errors.Is(err, ErrInvalidInputs) {
		t.Fatalf("overflow error = %v, want invalid inputs", err)
	}
}

func testPolicy() Policy {
	return Policy{
		MinWorkers:               0,
		WarmWorkers:              0,
		MaxWorkers:               20,
		InstanceCapacity:         Capacity{MilliCPU: 2_000, MemoryBytes: 4_000, ScratchBytes: 10_000, VMSlots: 2, BuildExecutors: 1},
		AllowedCompatibilityKeys: []string{"linux-amd64"},
		MaxScaleOutPerCycle:      10,
		MaxPendingWorkers:        10,
		MaxPackingItems:          10_000,
	}
}

func mustPlanner(t *testing.T, policy Policy) *Planner {
	t.Helper()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatal(err)
	}
	return planner
}

func largeDemand() Demand {
	return Demand{Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 2_000, MemoryBytes: 4_000, VMSlots: 2}, 10)}}
}

func workload(shape Capacity, count uint64) WorkloadBucket {
	return WorkloadBucket{CompatibilityKey: "linux-amd64", Shape: shape, Count: count}
}

func activeWorkers(count int) []Worker {
	workers := make([]Worker, 0, count)
	for index := range count {
		workers = append(workers, Worker{
			ID:          string(rune('a' + index)),
			State:       WorkerActive,
			ActivatedAt: testNow.Add(-time.Duration(index+1) * time.Hour),
		})
	}
	return workers
}
