package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	plannerTestGroupID           = "group-1"
	plannerTestRuntimeIdentityID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	plannerTestCPUConfigDigest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	plannerTestSubstrateFormat   = capacityapi.SubstrateFormatExt4
	plannerTestSubstrateContract = capacityapi.SubstrateContractExt4
)

var plannerTestNow = time.Unix(100, 0).UTC()

func TestPlanFreshRunUsesExactPrimaryPool(t *testing.T) {
	secondaryID := plannerTestUUID(1)
	primaryID := plannerTestUUID(2)
	secondary := plannerTestPool(secondaryID, "secondary")
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{secondary, primary},
		runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerFreshRun(11)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

	rows := []db.ListWorkerCapacityBinsRow{
		plannerBin(secondary, primaryID),
		plannerBin(primary, primaryID),
	}
	selected, ok := SelectRunWorker(rows, RunRequirements{
		Resources:    plannerRunResources(),
		Architecture: "x86_64",
	})
	if !ok || selected.WorkerPoolID != primaryID {
		t.Fatalf("SelectRunWorker() = pool %s, %v; want primary %s", plannerUUIDString(selected.WorkerPoolID), ok, plannerUUIDString(primaryID))
	}
}

func TestPlanWorkspaceExecScalesExactPrimaryPoolFromZero(t *testing.T) {
	secondaryID := plannerTestUUID(41)
	primaryID := plannerTestUUID(42)
	secondary := plannerTestPool(secondaryID, "secondary")
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{secondary, primary},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(43)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

func TestPlanWorkspaceExecAccountedSupplyBlocksExactRequestedPools(t *testing.T) {
	primaryID := plannerTestUUID(58)
	secondaryID := plannerTestUUID(59)
	outsideID := plannerTestUUID(60)
	primary := plannerTestPool(primaryID, "primary")
	secondary := plannerTestPool(secondaryID, "secondary")
	accounted := plannerWorkspaceExec(61)
	accounted.AccountedPoolIds = []pgtype.UUID{secondaryID, primaryID, outsideID, primaryID}
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary, secondary},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{accounted},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

func TestPlanWorkspaceExecAccountedSupplyDoesNotHideOrdinaryCandidate(t *testing.T) {
	primaryID := plannerTestUUID(62)
	primary := plannerTestPool(primaryID, "primary")
	accounted := plannerWorkspaceExec(63)
	accounted.AccountedPoolIds = []pgtype.UUID{primaryID}
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{accounted, plannerWorkspaceExec(64)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

func TestPlanWorkspaceExecUsesExistingCompatibleBin(t *testing.T) {
	primaryID := plannerTestUUID(44)
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary},
		bins:  []db.ListWorkerCapacityBinsRow{plannerBin(primary, primaryID)},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(45)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

func TestPlanWorkspaceExecCannotUseSecondaryOnlyRequest(t *testing.T) {
	primaryID := plannerTestUUID(53)
	secondaryID := plannerTestUUID(54)
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{
			plannerTestPool(primaryID, "primary"),
			plannerTestPool(secondaryID, "secondary"),
		},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(55)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
		plannerPoolRequest(secondaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	secondaryPlan := requirePoolPlan(t, plan, secondaryID)
	if secondaryPlan.RecommendedAdditionalWorkers != 0 || secondaryPlan.CompatibleQueuedItems != 0 {
		t.Fatalf("secondary pool plan = %+v", secondaryPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (capacityapi.CapacityIncompatibility{
		Reason: reasonProviderPool,
		Count:  1,
	}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanWorkspaceExecSharesFreshRunBinConstraints(t *testing.T) {
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
		{name: "runtime paused", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.RuntimePausedReason = pgtype.Text{String: "operator", Valid: true}
		}},
		{name: "run consumer", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableRunConsumers = 0
		}},
		{name: "runtime start", mutate: func(row *db.ListWorkerCapacityBinsRow) {
			row.AvailableRuntimeStarts = 0
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
				group: plannerTestGroup(primaryID),
				pools: []db.ListCapacityWorkerPoolsRow{primary},
				bins:  []db.ListWorkerCapacityBinsRow{constrained},
				execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(57)},
			}

			plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
				plannerPoolRequest(primaryID, 1),
			}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			poolPlan := requirePoolPlan(t, plan, primaryID)
			if poolPlan.RecommendedAdditionalWorkers != 1 || poolPlan.CompatibleQueuedItems != 1 {
				t.Fatalf("pool plan = %+v, want Workspace Exec rejected by constrained bin and packed on a fresh Worker", poolPlan)
			}
		})
	}
}

func TestPlanRunsAndWorkspaceExecsShareWorkerExecutionCounters(t *testing.T) {
	primaryID := plannerTestUUID(50)
	primary := plannerTestPool(primaryID, "primary")
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary},
		runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerFreshRun(51)},
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(52)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
		plannerPoolRequest(primaryID, 2),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 2 || poolPlan.CompatibleQueuedItems != 2 {
		t.Fatalf("pool plan = %+v, want separate Workers for one-slot Run and Workspace Exec", poolPlan)
	}
}

func TestDiscoverItemsScansWorkspaceExecIndependentlyFromRuns(t *testing.T) {
	primaryID := plannerTestUUID(46)
	runs := make([]db.ListQueuedRunPlanningCandidatesForScopesRow, maximumPlanningCandidates+1)
	for index := range runs {
		runs[index] = plannerFreshRun(byte(index))
	}
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		runs:  runs,
		execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{plannerWorkspaceExec(47)},
	}

	items, accounted, complete, err := discoverItems(context.Background(), store, store.group, plannerTestNow.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("discoverItems complete = true, want false for truncated Run scan")
	}
	if len(accounted) != 0 {
		t.Fatalf("accounted pools = %+v, want none", accounted)
	}
	if len(items) != int(maximumPlanningCandidates)+1 {
		t.Fatalf("items = %d, want %d Runs plus independent Workspace Exec", len(items), maximumPlanningCandidates+1)
	}
	if got := items[len(items)-1].key; got != fmt.Sprintf("workspace-exec:%x", plannerTestUUID(47).Bytes) {
		t.Fatalf("last item key = %q, want Workspace Exec", got)
	}
}

func TestDiscoverItemsMarksTruncatedWorkspaceExecScanIncomplete(t *testing.T) {
	primaryID := plannerTestUUID(48)
	execs := make([]db.ListPendingWorkspaceExecCapacityCandidatesRow, maximumPlanningCandidates+1)
	for index := range execs {
		execs[index] = plannerWorkspaceExec(byte(index))
	}
	store := plannerStore{group: plannerTestGroup(primaryID), execs: execs}

	items, accounted, complete, err := discoverItems(context.Background(), store, store.group, plannerTestNow.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if complete || len(items) != int(maximumPlanningCandidates) {
		t.Fatalf("complete = %v, items = %d", complete, len(items))
	}
	if len(accounted) != 0 {
		t.Fatalf("accounted pools = %+v, want none", accounted)
	}
}

func TestWorkspaceExecItemRejectsMalformedSandboxDemand(t *testing.T) {
	item := workspaceExecItem(db.ListPendingWorkspaceExecCapacityCandidatesRow{
		ProcessID: plannerTestUUID(49), WorkspaceManifestVersion: deployment.DeploymentPlanFormatVersion,
		WorkspaceManifest: []byte(`{"resources":{"milliCpu":0,"memoryMiB":1024}}`),
	})
	if item.reason != reasonInvalidWorkload {
		t.Fatalf("reason = %q, want %q", item.reason, reasonInvalidWorkload)
	}
	item = workspaceExecItem(db.ListPendingWorkspaceExecCapacityCandidatesRow{
		ProcessID: plannerTestUUID(49), WorkspaceManifestVersion: deployment.DeploymentPlanFormatVersion + 1,
		WorkspaceManifest: plannerWorkspaceManifest(),
	})
	if item.reason != reasonInvalidWorkload {
		t.Fatalf("wrong-version reason = %q, want %q", item.reason, reasonInvalidWorkload)
	}
}

func TestRunWorkerCapacityPressureCandidatesIgnoreOnlyDynamicCapacity(t *testing.T) {
	primaryID := plannerTestUUID(1)
	secondaryID := plannerTestUUID(2)
	primary := plannerBin(plannerTestPool(primaryID, "primary"), primaryID)
	primary.AvailableCPUMillis = 0
	primary.AvailableMemoryBytes = 0
	primary.AvailableGuestEphemeralDiskBytes = 0
	primary.AvailableVMSlots = 0
	primary.AvailableRunConsumers = 0
	primary.AvailableRuntimeStarts = 0
	secondary := plannerBin(plannerTestPool(secondaryID, "secondary"), primaryID)
	paused := primary
	paused.WorkerInstanceID = plannerTestUUID(103)
	paused.RunPausedReason = pgtype.Text{String: "operator", Valid: true}

	request := RunRequirements{Resources: plannerRunResources(), Architecture: "x86_64"}
	if _, ok := SelectRunWorker([]db.ListWorkerCapacityBinsRow{primary}, request); ok {
		t.Fatal("ordinary selection accepted a Worker with no dynamic capacity")
	}
	candidates := RunWorkerCapacityPressureCandidates(
		[]db.ListWorkerCapacityBinsRow{secondary, primary, paused},
		request,
	)
	if len(candidates) != 1 || candidates[0].WorkerInstanceID != primary.WorkerInstanceID {
		t.Fatalf("capacity pressure candidates = %+v, want only hard-compatible primary Worker", candidates)
	}

	tooLarge := request
	tooLarge.Resources.CPUMillis = primary.PerVMCPUMillis + 1
	if candidates := RunWorkerCapacityPressureCandidates([]db.ListWorkerCapacityBinsRow{primary}, tooLarge); len(candidates) != 0 {
		t.Fatalf("per-VM incompatible capacity pressure candidates = %+v", candidates)
	}
}

func TestRunWorkerCapacityPressureCandidatesPreserveRestoreIdentity(t *testing.T) {
	primaryID := plannerTestUUID(1)
	compatible := plannerBin(plannerTestPool(primaryID, "compatible"), primaryID)
	compatible.AvailableVMSlots = 0
	compatible.AvailableRunConsumers = 0
	compatible.AvailableRuntimeStarts = 0
	requirements := plannerRestoreRequirements()
	request := RunRequirements{
		Resources: requirements.Resources, Architecture: "x86_64",
		WorkerGroupID: requirements.WorkerGroupID, RuntimeIdentityID: requirements.RuntimeIdentityID,
		VCPUCount: requirements.VCPUCount, CPUConfigDigest: requirements.CPUConfigDigest,
		SubstrateFormat: requirements.SubstrateFormat, SubstrateContract: requirements.SubstrateContract,
	}
	wrongCPU := compatible
	wrongCPU.WorkerInstanceID = plannerTestUUID(102)
	wrongCPU.CPUShapeConfigDigests = []string{plannerDigest('c')}

	candidates := RunWorkerCapacityPressureCandidates([]db.ListWorkerCapacityBinsRow{wrongCPU, compatible}, request)
	if len(candidates) != 1 || candidates[0].WorkerInstanceID != compatible.WorkerInstanceID {
		t.Fatalf("restore capacity pressure candidates = %+v, want exact restore-compatible Worker", candidates)
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
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary, secondary},
		runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerRestoreRun(13, requirements)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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
	fullPrimary.AvailableRunConsumers = 0
	fullPrimary.AvailableRuntimeStarts = 0
	requirements := plannerRestoreRequirements()
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{primary, secondary},
		bins:  []db.ListWorkerCapacityBinsRow{fullPrimary},
		runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerRestoreRun(14, requirements)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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

func TestPlanAssignsRestoreOnceInDeterministicPoolOrder(t *testing.T) {
	firstID := plannerTestUUID(1)
	secondID := plannerTestUUID(2)
	first := plannerTestPool(firstID, "first")
	first.ActiveWorkers = 2
	second := plannerTestPool(secondID, "second")
	second.ActiveWorkers = 3
	requirements := plannerRestoreRequirements()
	run := plannerRestoreRun(15, requirements)
	group := plannerTestGroup(firstID)

	forward, err := Plan(context.Background(), plannerStore{
		group: group, pools: []db.ListCapacityWorkerPoolsRow{first, second}, runs: []db.ListQueuedRunPlanningCandidatesForScopesRow{run},
	}, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
		plannerPoolRequest(firstID, 1), plannerPoolRequest(secondID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := Plan(context.Background(), plannerStore{
		group: group, pools: []db.ListCapacityWorkerPoolsRow{second, first}, runs: []db.ListQueuedRunPlanningCandidatesForScopesRow{run},
	}, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
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
	if firstPlan.CompatibleQueuedItems != 1 || firstPlan.RecommendedAdditionalWorkers != 1 || !firstPlan.ScaleInBlocked {
		t.Fatalf("first pool plan = %+v", firstPlan)
	}
	if secondPlan.CompatibleQueuedItems != 0 || secondPlan.RecommendedAdditionalWorkers != 0 || secondPlan.ScaleInBlocked {
		t.Fatalf("second pool plan = %+v", secondPlan)
	}
	if got := firstPlan.CompatibleQueuedItems + secondPlan.CompatibleQueuedItems; got != 1 {
		t.Fatalf("compatible item count = %d, want exactly 1", got)
	}
}

func TestPlanReportsPerPoolSaturationAndUnmatchedDemand(t *testing.T) {
	primaryID := plannerTestUUID(1)
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{plannerTestPool(primaryID, "primary")},
		runs: []db.ListQueuedRunPlanningCandidatesForScopesRow{
			plannerFreshRun(21), plannerFreshRun(22), plannerFreshRun(23),
		},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
		plannerPoolRequest(primaryID, 1),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 1 || poolPlan.CompatibleQueuedItems != 1 || !poolPlan.Saturated {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (capacityapi.CapacityIncompatibility{Reason: reasonProviderSaturated, Count: 2}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestPlanAcceptsZeroAdditionalWorkerBudget(t *testing.T) {
	primaryID := plannerTestUUID(1)
	store := plannerStore{
		group: plannerTestGroup(primaryID),
		pools: []db.ListCapacityWorkerPoolsRow{plannerTestPool(primaryID, "primary")},
		runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerFreshRun(24)},
	}

	plan, err := Plan(context.Background(), store, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
		plannerPoolRequest(primaryID, 0),
	}}, plannerTestNow)
	if err != nil {
		t.Fatal(err)
	}

	poolPlan := requirePoolPlan(t, plan, primaryID)
	if poolPlan.RecommendedAdditionalWorkers != 0 || poolPlan.CompatibleQueuedItems != 0 || !poolPlan.Saturated {
		t.Fatalf("pool plan = %+v", poolPlan)
	}
	if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (capacityapi.CapacityIncompatibility{Reason: reasonProviderSaturated, Count: 1}) {
		t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
	}
}

func TestRestoreCompatibilityParityBetweenPlannerAndImmediateSelection(t *testing.T) {
	requirements := plannerRestoreRequirements()
	request := RunRequirements{
		Resources:         requirements.Resources,
		Architecture:      "x86_64",
		WorkerGroupID:     requirements.WorkerGroupID,
		RuntimeIdentityID: requirements.RuntimeIdentityID,
		VCPUCount:         requirements.VCPUCount,
		CPUConfigDigest:   requirements.CPUConfigDigest,
		SubstrateFormat:   requirements.SubstrateFormat,
		SubstrateContract: requirements.SubstrateContract,
	}

	compatible := plannerTestPool(plannerTestUUID(1), "compatible")
	wrongCPU := plannerTestPool(plannerTestUUID(2), "wrong-cpu")
	wrongCPU.CPUShapeConfigDigests = []string{plannerDigest('c')}
	tooSmall := plannerTestPool(plannerTestUUID(3), "too-small")
	tooSmall.PerVMCPUMillis = pgtype.Int8{Int64: requirements.Resources.CPUMillis - 1, Valid: true}
	tooSmall.CapacityCPUMillis = tooSmall.PerVMCPUMillis

	for _, test := range []struct {
		name string
		pool db.ListCapacityWorkerPoolsRow
	}{
		{name: "compatible", pool: compatible},
		{name: "cpu shape mismatch", pool: wrongCPU},
		{name: "per-VM capacity mismatch", pool: tooSmall},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := CanRestore(requirements, plannerPool(test.pool))
			_, immediate := SelectRunWorker([]db.ListWorkerCapacityBinsRow{
				plannerBin(test.pool, compatible.ID),
			}, request)
			if immediate != want {
				t.Fatalf("SelectRunWorker compatible = %v, CanRestore = %v", immediate, want)
			}

			plan, err := Plan(context.Background(), plannerStore{
				group: plannerTestGroup(compatible.ID),
				pools: []db.ListCapacityWorkerPoolsRow{test.pool},
				runs:  []db.ListQueuedRunPlanningCandidatesForScopesRow{plannerRestoreRun(31, requirements)},
			}, plannerTestGroupID, capacityapi.CapacityPlanRequest{Pools: []capacityapi.CapacityPoolRequest{
				plannerPoolRequest(test.pool.ID, 1),
			}}, plannerTestNow)
			if err != nil {
				t.Fatal(err)
			}
			poolPlan := requirePoolPlan(t, plan, test.pool.ID)
			planned := poolPlan.CompatibleQueuedItems == 1 && poolPlan.RecommendedAdditionalWorkers == 1
			if planned != want {
				t.Fatalf("planner compatible = %v, CanRestore = %v; plan = %+v", planned, want, plan)
			}
			if want {
				if len(plan.UnmatchedDemand) != 0 {
					t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
				}
			} else if len(plan.UnmatchedDemand) != 1 || plan.UnmatchedDemand[0] != (capacityapi.CapacityIncompatibility{Reason: reasonRuntimeCompatibility, Count: 1}) {
				t.Fatalf("unmatched demand = %+v", plan.UnmatchedDemand)
			}
		})
	}
}

func plannerTestGroup(primaryRunPoolID pgtype.UUID) db.WorkerGroup {
	return db.WorkerGroup{
		ID: plannerTestGroupID, Name: "default", RegionID: "us-east-1", State: string(capacityapi.WorkerGroupStatusActive),
		PrimaryPoolID: primaryRunPoolID,
	}
}

func plannerTestPool(id pgtype.UUID, name string) db.ListCapacityWorkerPoolsRow {
	return db.ListCapacityWorkerPoolsRow{
		ID: id, WorkerGroupID: plannerTestGroupID, Name: name,
		RuntimeIdentityID:               pgtype.Text{String: plannerTestRuntimeIdentityID, Valid: true},
		SubstrateFormat:                 pgtype.Text{String: plannerTestSubstrateFormat, Valid: true},
		SubstrateContract:               pgtype.Text{String: plannerTestSubstrateContract, Valid: true},
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
	shapes := make([]capacityapi.CPUShape, len(row.CPUShapeVCPUCounts))
	for index := range row.CPUShapeVCPUCounts {
		shapes[index] = capacityapi.CPUShape{
			VCPUCount: row.CPUShapeVCPUCounts[index], CPUConfigDigest: row.CPUShapeConfigDigests[index],
		}
	}
	return Pool{
		WorkerGroupID: row.WorkerGroupID, RuntimeIdentityID: row.RuntimeIdentityID.String,
		SubstrateFormat: row.SubstrateFormat.String, SubstrateContract: row.SubstrateContract.String,
		PerVM: capacityapi.ResourceVector{
			CPUMillis: row.PerVMCPUMillis.Int64, MemoryBytes: row.PerVMMemoryBytes.Int64,
			GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes.Int64,
		},
		CPUShapes: shapes,
	}
}

func plannerBin(row db.ListCapacityWorkerPoolsRow, primaryRunPoolID pgtype.UUID) db.ListWorkerCapacityBinsRow {
	return db.ListWorkerCapacityBinsRow{
		WorkerGroupID: plannerTestGroupID, PrimaryPoolID: primaryRunPoolID, WorkerPoolID: row.ID,
		WorkerInstanceID:  plannerTestUUID(row.ID.Bytes[15] + 100),
		RuntimeIdentityID: row.RuntimeIdentityID, RuntimeArch: "x86_64", VMRuntimeContract: capacityapi.RuntimeContract,
		SubstrateFormat: row.SubstrateFormat.String, SubstrateContract: row.SubstrateContract.String,
		PerVMCPUMillis: row.PerVMCPUMillis.Int64, PerVMMemoryBytes: row.PerVMMemoryBytes.Int64,
		PerVMGuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes.Int64,
		AvailableCPUMillis:           row.CapacityCPUMillis.Int64, AvailableMemoryBytes: row.CapacityMemoryBytes.Int64,
		AvailableGuestEphemeralDiskBytes: row.CapacityGuestEphemeralDiskBytes.Int64,
		AvailableVMSlots:                 int64(row.MaxVMSlots.Int32), AvailableRunConsumers: int64(row.MaxVMSlots.Int32),
		AvailableRuntimeStarts: int64(row.MaxVMSlots.Int32),
		CPUShapeVCPUCounts:     append([]int32(nil), row.CPUShapeVCPUCounts...),
		CPUShapeConfigDigests:  append([]string(nil), row.CPUShapeConfigDigests...),
	}
}

func plannerFreshRun(seed byte) db.ListQueuedRunPlanningCandidatesForScopesRow {
	return db.ListQueuedRunPlanningCandidatesForScopesRow{
		RunID: plannerTestUUID(seed), WorkspaceManifestVersion: deployment.DeploymentPlanFormatVersion,
		WorkspaceManifest: plannerWorkspaceManifest(),
	}
}

func plannerWorkspaceExec(seed byte) db.ListPendingWorkspaceExecCapacityCandidatesRow {
	return db.ListPendingWorkspaceExecCapacityCandidatesRow{
		ProcessID: plannerTestUUID(seed), WorkspaceManifestVersion: deployment.DeploymentPlanFormatVersion,
		WorkspaceManifest: plannerWorkspaceManifest(),
	}
}

func plannerRestoreRun(seed byte, requirements RestoreRequirements) db.ListQueuedRunPlanningCandidatesForScopesRow {
	return db.ListQueuedRunPlanningCandidatesForScopesRow{
		RunID: plannerTestUUID(seed), WorkspaceManifestVersion: deployment.DeploymentPlanFormatVersion,
		WorkspaceManifest:     plannerWorkspaceManifest(),
		RequiredWorkerGroupID: requirements.WorkerGroupID, RequiredRuntimeIdentityID: requirements.RuntimeIdentityID,
		RequiredVMVCPUCount: requirements.VCPUCount, RequiredCPUConfigDigest: requirements.CPUConfigDigest,
		RequiredCPUMillis: requirements.Resources.CPUMillis, RequiredMemoryBytes: requirements.Resources.MemoryBytes,
		RequiredGuestEphemeralDiskBytes: requirements.Resources.GuestEphemeralDiskBytes,
		RequiredSubstrateFormat:         requirements.SubstrateFormat, RequiredSubstrateContract: requirements.SubstrateContract,
	}
}

func plannerRestoreRequirements() RestoreRequirements {
	return RestoreRequirements{
		WorkerGroupID: plannerTestGroupID, RuntimeIdentityID: plannerTestRuntimeIdentityID,
		VCPUCount: 1, CPUConfigDigest: plannerTestCPUConfigDigest,
		SubstrateFormat: plannerTestSubstrateFormat, SubstrateContract: plannerTestSubstrateContract,
		Resources: plannerRunResources(),
	}
}

func plannerRunResources() capacityapi.ResourceVector {
	return capacityapi.ResourceVector{
		CPUMillis: 1000, MemoryBytes: 1 << 30, GuestEphemeralDiskBytes: 32 << 30, VMSlots: 1,
	}
}

func plannerWorkspaceManifest() []byte {
	result, err := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{
		MilliCPU: 1000, MemoryMiB: 1024,
	}})
	if err != nil {
		panic(err)
	}
	return result
}

func plannerPoolRequest(id pgtype.UUID, max int32) capacityapi.CapacityPoolRequest {
	return capacityapi.CapacityPoolRequest{PoolID: plannerUUIDString(id), MaxAdditionalWorkers: max}
}

func requirePoolPlan(t *testing.T, plan capacityapi.CapacityPlanResponse, id pgtype.UUID) capacityapi.CapacityPoolPlan {
	t.Helper()
	want := plannerUUIDString(id)
	for _, pool := range plan.Pools {
		if pool.PoolID == want {
			return pool
		}
	}
	t.Fatalf("pool %s is missing from %+v", want, plan.Pools)
	return capacityapi.CapacityPoolPlan{}
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
	group  db.WorkerGroup
	pools  []db.ListCapacityWorkerPoolsRow
	bins   []db.ListWorkerCapacityBinsRow
	scopes []db.ListQueuedRunEligibleScopesRow
	usage  []db.ListQueuedRunPlanningUsageRow
	runs   []db.ListQueuedRunPlanningCandidatesForScopesRow
	execs  []db.ListPendingWorkspaceExecCapacityCandidatesRow
}

func (s plannerStore) GetWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
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

func (s plannerStore) ListQueuedRunEligibleScopes(_ context.Context, arg db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	scopes := s.scopes
	if len(scopes) == 0 && len(s.runs) > 0 {
		scopes = []db.ListQueuedRunEligibleScopesRow{{SortKey: "00000001", RegionID: s.group.RegionID, QueueName: "run"}}
	}
	result := make([]db.ListQueuedRunEligibleScopesRow, 0, min(len(scopes), int(arg.RowLimit)))
	for index, scope := range scopes {
		if scope.SortKey == "" {
			scope.SortKey = fmt.Sprintf("%08d", index+1)
		}
		if scope.SortKey <= arg.AfterSortKey || arg.RegionFilter != "" && scope.RegionID != arg.RegionFilter {
			continue
		}
		result = append(result, scope)
		if len(result) == int(arg.RowLimit) {
			break
		}
	}
	return result, nil
}

func (s plannerStore) ListQueuedRunPlanningUsage(_ context.Context, arg db.ListQueuedRunPlanningUsageParams) ([]db.ListQueuedRunPlanningUsageRow, error) {
	result := make([]db.ListQueuedRunPlanningUsageRow, len(arg.EnvironmentIds))
	for index := range result {
		if index < len(s.usage) {
			result[index] = s.usage[index]
		}
		result[index].ScopeOrdinal = int64(index + 1)
	}
	return result, nil
}

func (s plannerStore) ListQueuedRunPlanningCandidatesForScopes(context.Context, db.ListQueuedRunPlanningCandidatesForScopesParams) ([]db.ListQueuedRunPlanningCandidatesForScopesRow, error) {
	result := make([]db.ListQueuedRunPlanningCandidatesForScopesRow, len(s.runs))
	copy(result, s.runs)
	for index := range result {
		result[index].ScopeOrdinal = 1
	}
	return result, nil
}

func (s plannerStore) ListPendingWorkspaceExecCapacityCandidates(_ context.Context, arg db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error) {
	limit := min(len(s.execs), int(arg.RowLimit))
	result := make([]db.ListPendingWorkspaceExecCapacityCandidatesRow, limit)
	copy(result, s.execs[:limit])
	return result, nil
}
