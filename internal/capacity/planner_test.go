package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPlanPacksExactShapesIntoCurrentAndTemplateBins(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, err := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs: []db.ListQueuedRunDispatchCandidatesForScopeRow{
			{RunID: testUUID(1), WorkspaceManifest: workspace},
			{RunID: testUUID(2), WorkspaceManifest: workspace},
			{RunID: testUUID(3), WorkspaceManifest: workspace},
		},
		bins: []db.ListWorkerCapacityBinsRow{{
			SupportsRun: true, RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0",
			RuntimeIdentityID: pgtype.Text{String: "runtime", Valid: true},
			PerVMCPUMillis:    2000, PerVMMemoryBytes: 2 << 30, PerVMGuestEphemeralDiskBytes: 32 << 30,
			AvailableCPUMillis: 1000, AvailableMemoryBytes: 1 << 30, AvailableGuestEphemeralDiskBytes: 32 << 30,
			AvailableVMSlots: 1, AvailableRunConsumers: 1, AvailableRuntimeStarts: 1,
		}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{
		Worker: manifest, MaxAdditionalWorkers: 4,
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 1 || plan.CompatibleQueuedItems != 3 || !plan.Complete || plan.Saturated {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanReportsRetainedRuntimeAndSaturation(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs: []db.ListQueuedRunDispatchCandidatesForScopeRow{
			{RunID: testUUID(1), WorkspaceManifest: workspace},
			{RunID: testUUID(2), WorkspaceManifest: workspace},
			{RunID: testUUID(3), WorkspaceManifest: workspace},
			{RunID: testUUID(4), WorkspaceManifest: workspace},
			{RunID: testUUID(5), WorkspaceManifest: workspace, RequiresRetainedRuntime: true},
		},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{
		Worker: manifest, MaxAdditionalWorkers: 1,
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 1 || !plan.Saturated || !plan.ScaleInBlocked || plan.CompatibleQueuedItems != 4 || plan.IncompatibleQueuedItems != 1 ||
		len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonRetainedRuntime {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanRejectsRestoreWithDifferentRuntimeIdentity(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs: []db.ListQueuedRunDispatchCandidatesForScopeRow{{
			RunID: testUUID(1), WorkspaceManifest: workspace,
			RequiredRuntimeIdentityID: "another-runtime", RequiredSubstrateFormat: "ext4", RequiredSubstrateContract: "substrate",
		}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{
		Worker: manifest, MaxAdditionalWorkers: 1,
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 0 || !plan.ScaleInBlocked || plan.CompatibleQueuedItems != 0 || plan.IncompatibleQueuedItems != 1 ||
		len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonRuntimeCompatibility {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanRejectsTemplateBelowWorkerGroupContract(t *testing.T) {
	manifest := plannerTestManifest(t)
	store := plannerStore{group: db.WorkerGroup{
		ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true,
		RequiredCPUMillis: manifest.Capacity.CPUMillis + 1,
	}}
	_, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{
		Worker: manifest, MaxAdditionalWorkers: 1,
	}, time.Unix(100, 0))
	if !errors.Is(err, ErrInvalidPlanRequest) {
		t.Fatalf("Plan() error = %v, want ErrInvalidPlanRequest", err)
	}

	store.group.RequiredCPUMillis = 0
	store.group.AllowsRun = false
	store.group.AllowsBuild = true
	_, err = Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{
		Worker: manifest, MaxAdditionalWorkers: 1,
	}, time.Unix(100, 0))
	if !errors.Is(err, ErrInvalidPlanRequest) {
		t.Fatalf("Plan() role error = %v, want ErrInvalidPlanRequest", err)
	}
}

func TestPlanClassifiesDemandBeyondQueueConcurrencyHeadroom(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group: db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{
			RegionID: "us-east-1", QueueName: "run", ActiveRuns: 1,
		}},
		runs: []db.ListQueuedRunDispatchCandidatesForScopeRow{
			{RunID: testUUID(1), WorkspaceManifest: workspace, QueueConcurrencyLimit: pgtype.Int8{Int64: 2, Valid: true}},
			{RunID: testUUID(2), WorkspaceManifest: workspace, QueueConcurrencyLimit: pgtype.Int8{Int64: 2, Valid: true}},
			{RunID: testUUID(3), WorkspaceManifest: workspace, QueueConcurrencyLimit: pgtype.Int8{Int64: 2, Valid: true}},
		},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.CompatibleQueuedItems != 1 || !plan.ScaleInBlocked || plan.IncompatibleQueuedItems != 2 || len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonQueueConcurrency {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanEvaluatesMixedCandidateQueueLimitsIndividually(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run", ActiveRuns: 1}},
		runs: []db.ListQueuedRunDispatchCandidatesForScopeRow{
			{RunID: testUUID(1), WorkspaceManifest: workspace, QueueConcurrencyLimit: pgtype.Int8{Int64: 4, Valid: true}},
			{RunID: testUUID(2), WorkspaceManifest: workspace, QueueConcurrencyLimit: pgtype.Int8{Int64: 2, Valid: true}},
			{RunID: testUUID(3), WorkspaceManifest: workspace},
		},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.CompatibleQueuedItems != 2 || plan.IncompatibleQueuedItems != 1 || len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonQueueConcurrency {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanHonorsPinnedPreparedQueueLimit(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run", PreparedRuns: 1, PreparedLimit: 1}},
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.CompatibleQueuedItems != 0 || plan.IncompatibleQueuedItems != 1 || len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonQueueConcurrency {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanPagesRunScopesBeforeConcludingDemandIsEmpty(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	scopes := make([]db.ListQueuedRunCandidateScopesRow, 0, planningScopePageSize+1)
	for index := range planningScopePageSize {
		scopes = append(scopes, db.ListQueuedRunCandidateScopesRow{RegionID: "us-east-1", QueueName: fmt.Sprintf("empty-%03d", index)})
	}
	scopes = append(scopes, db.ListQueuedRunCandidateScopesRow{RegionID: "us-east-1", QueueName: "target"})
	store := &pagingPlannerStore{plannerStore: plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: scopes,
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
	}}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if store.scopeCalls != 2 || plan.CompatibleQueuedItems != 1 || plan.RecommendedAdditionalWorkers != 1 || !plan.Complete {
		t.Fatalf("scope calls = %d, plan = %+v", store.scopeCalls, plan)
	}
}

func TestPlanRunDiagnosticsDoNotConsumeBuildSamplingBudget(t *testing.T) {
	manifest := plannerTestManifest(t)
	manifest.SupportsBuild = true
	manifest.Capacity.CPUMillis = 3000
	manifest.Capacity.MemoryBytes = 4 << 30
	manifest.Capacity.BuildExecutors = 1
	manifest.PerVM.CPUMillis = 3000
	manifest.PerVM.MemoryBytes = 4 << 30
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	runs := make([]db.ListQueuedRunDispatchCandidatesForScopeRow, maximumPlanningCandidates)
	for index := range runs {
		runs[index] = db.ListQueuedRunDispatchCandidatesForScopeRow{
			RunID: testUUID(byte(index)), WorkspaceManifest: workspace,
			QueueConcurrencyLimit: pgtype.Int8{Int64: 1, Valid: true},
		}
	}
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true, AllowsBuild: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run", ActiveRuns: 1}},
		runs:   runs, builds: []db.ListQueuedDeploymentBuildCandidatesRow{{DeploymentID: testUUID(1)}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 1}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 1 || plan.CompatibleQueuedItems != 1 || plan.IncompatibleQueuedItems != int64(maximumPlanningCandidates) {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanClassifiesOverflowingRunMemoryAsInvalid(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{
		MilliCPU: 1000, MemoryMiB: math.MaxInt64/mebibyte + 1,
	}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 1}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 0 || plan.ScaleInBlocked || plan.CompatibleQueuedItems != 0 || plan.IncompatibleQueuedItems != 1 ||
		len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonInvalidWorkload {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanDoesNotPinFleetForOversizedDemand(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{
		MilliCPU: manifest.PerVM.CPUMillis + 1, MemoryMiB: 1024,
	}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 1}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 0 || plan.ScaleInBlocked || plan.CompatibleQueuedItems != 0 || plan.IncompatibleQueuedItems != 1 ||
		len(plan.Incompatibilities) != 1 || plan.Incompatibilities[0].Reason != reasonPerInstanceResources {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanPacksBuildDemandIntoCurrentAndTemplateBins(t *testing.T) {
	manifest := plannerTestManifest(t)
	manifest.SupportsBuild = true
	manifest.Capacity.CPUMillis = 6000
	manifest.Capacity.MemoryBytes = 8 << 30
	manifest.Capacity.BuildExecutors = 1
	manifest.PerVM.CPUMillis = 3000
	manifest.PerVM.MemoryBytes = 4 << 30
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	store := plannerStore{
		group: db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true, AllowsBuild: true},
		builds: []db.ListQueuedDeploymentBuildCandidatesRow{
			{DeploymentID: testUUID(1)}, {DeploymentID: testUUID(2)}, {DeploymentID: testUUID(3)},
		},
		bins: []db.ListWorkerCapacityBinsRow{{
			SupportsBuild: true, RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0",
			PerVMCPUMillis: 3000, PerVMMemoryBytes: 4 << 30, PerVMGuestEphemeralDiskBytes: 32 << 30,
			AvailableCPUMillis: 3000, AvailableMemoryBytes: 4 << 30, AvailableGuestEphemeralDiskBytes: 32 << 30,
			AvailableBuildExecutors: 1,
		}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 2 || plan.CompatibleQueuedItems != 3 || plan.IncompatibleQueuedItems != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanSuppressesScaleOutWhenCurrentSupplyIsIncomplete(t *testing.T) {
	manifest := plannerTestManifest(t)
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	bins := make([]db.ListWorkerCapacityBinsRow, maximumPlanningWorkers+1)
	for index := range bins {
		workerID := pgtype.UUID{Valid: true}
		workerID.Bytes[14] = byte(index >> 8)
		workerID.Bytes[15] = byte(index)
		bins[index] = db.ListWorkerCapacityBinsRow{
			WorkerInstanceID: workerID,
			SupportsRun:      true, RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0",
			RuntimeIdentityID: pgtype.Text{String: "runtime", Valid: true},
			PerVMCPUMillis:    2000, PerVMMemoryBytes: 2 << 30, PerVMGuestEphemeralDiskBytes: 32 << 30,
		}
	}
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
		bins:   bins,
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Complete || plan.RecommendedAdditionalWorkers != 0 || plan.CompatibleQueuedItems != 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanPreservesMixedRunAndBuildShapes(t *testing.T) {
	manifest := plannerTestManifest(t)
	manifest.SupportsBuild = true
	manifest.Capacity.CPUMillis = 6000
	manifest.Capacity.MemoryBytes = 8 << 30
	manifest.Capacity.BuildExecutors = 1
	manifest.PerVM.CPUMillis = 3000
	manifest.PerVM.MemoryBytes = 4 << 30
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	workspace, _ := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}})
	store := plannerStore{
		group:  db.WorkerGroup{ID: "group-1", Name: "default", RegionID: "us-east-1", State: "active", AllowsRun: true, AllowsBuild: true},
		scopes: []db.ListQueuedRunCandidateScopesRow{{RegionID: "us-east-1", QueueName: "run"}},
		runs:   []db.ListQueuedRunDispatchCandidatesForScopeRow{{RunID: testUUID(1), WorkspaceManifest: workspace}},
		builds: []db.ListQueuedDeploymentBuildCandidatesRow{{DeploymentID: testUUID(2)}, {DeploymentID: testUUID(3)}},
	}
	plan, err := Plan(context.Background(), store, "group-1", capacityapi.CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecommendedAdditionalWorkers != 2 || plan.CompatibleQueuedItems != 3 {
		t.Fatalf("plan = %+v", plan)
	}
}

func plannerTestManifest(t *testing.T) capacityapi.WorkerReleaseManifest {
	t.Helper()
	runtime := capacityapi.RuntimeProfile{
		Arch: "x86_64", Contract: "helmr.vm-runtime.v0",
		KernelDigest:    "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:    "sha256:" + strings.Repeat("3", 64),
	}
	runtime.ID, _ = runtime.ExpectedID()
	manifest := capacityapi.WorkerReleaseManifest{
		Schema: capacityapi.WorkerReleaseManifestSchema, WorkerVersion: "0123456789abcdef0123456789abcdef01234567", SupportsRun: true,
		Runtime:          runtime,
		Substrate:        capacityapi.SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity:         capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 2},
		PerVM:            capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 32 << 30},
		MaxRuntimeStarts: 2,
	}
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	return manifest
}

type plannerStore struct {
	group  db.WorkerGroup
	bins   []db.ListWorkerCapacityBinsRow
	scopes []db.ListQueuedRunCandidateScopesRow
	runs   []db.ListQueuedRunDispatchCandidatesForScopeRow
	builds []db.ListQueuedDeploymentBuildCandidatesRow
}

type pagingPlannerStore struct {
	plannerStore
	scopeCalls int
}

func (s *pagingPlannerStore) ListQueuedRunCandidateScopes(ctx context.Context, arg db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	s.scopeCalls++
	return s.plannerStore.ListQueuedRunCandidateScopes(ctx, arg)
}

func (s *pagingPlannerStore) ListQueuedRunDispatchCandidatesForScope(_ context.Context, arg db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error) {
	if arg.QueueName != "target" {
		return nil, nil
	}
	return s.runs, nil
}

func (s plannerStore) GetWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
	return s.group, nil
}
func (s plannerStore) ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error) {
	return s.bins, nil
}
func (s plannerStore) ListQueuedRunCandidateScopes(_ context.Context, arg db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	result := make([]db.ListQueuedRunCandidateScopesRow, 0, min(len(s.scopes), int(arg.RowLimit)))
	for index, scope := range s.scopes {
		if scope.SortKey == "" {
			scope.SortKey = fmt.Sprintf("%08d", index)
		}
		if scope.SortKey <= arg.AfterSortKey || (arg.RegionFilter != "" && scope.RegionID != arg.RegionFilter) {
			continue
		}
		result = append(result, scope)
		if len(result) == int(arg.RowLimit) {
			break
		}
	}
	return result, nil
}
func (s plannerStore) ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error) {
	return s.runs, nil
}
func (s plannerStore) ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error) {
	return s.builds, nil
}

func testUUID(last byte) pgtype.UUID {
	value := pgtype.UUID{Valid: true}
	value.Bytes[15] = last
	return value
}
