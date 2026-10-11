package workergroup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maximumPlanningCandidates = int32(5000)
	maximumPlanningWorkers    = 1000
	maximumPlanningPools      = 64
	maximumAdditionalWorkers  = int32(1000)
	mebibyte                  = int64(1024 * 1024)
)

var ErrInvalidPlanRequest = errors.New("invalid capacity plan request")

const (
	reasonRunRole              = "worker_does_not_support_run"
	reasonPerInstanceResources = "per_instance_resources"
	reasonRuntimeCompatibility = "runtime_compatibility"
	reasonInvalidWorkload      = "invalid_workload_requirements"
	reasonPrimaryPool          = "primary_pool_unavailable"
	reasonProviderPool         = "provider_pool_unavailable"
	reasonProviderSaturated    = "provider_capacity_saturated"
)

type PlanStore interface {
	ListAllocationPlanningChargedPools(context.Context, pgtype.UUID) ([]pgtype.UUID, error)
	GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error)
	ListCapacityWorkerPools(context.Context, db.ListCapacityWorkerPoolsParams) ([]db.ListCapacityWorkerPoolsRow, error)
	ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error)
	ListAllocationPlanningDemand(context.Context, db.ListAllocationPlanningDemandParams) ([]db.ListAllocationPlanningDemandRow, error)
}

type environmentBudget struct {
	cpu, memory, residents int64
}

type item struct {
	environment  [16]byte
	budget       environmentBudget
	computer     bool
	role         string
	resources    ResourceVector
	targetPoolID pgtype.UUID
	restore      *RestoreRequirements
	vmPlatformID string
	reason       string
	key          string
}

type bin struct {
	workerGroupID   uuid.UUID
	workerPoolID    pgtype.UUID
	resources       ResourceVector
	instanceStarts  int64
	supportsRun     bool
	runtimeArch     string
	runtimeContract string
	vmPlatformID    string
	runPaused       bool
	instancePaused  bool
	perVM           ResourceVector
	cpuShapes       []vmplatform.CPUShape
}

type RestoreRequirements struct {
	WorkerGroupID   uuid.UUID
	VMPlatformID    string
	VCPUCount       int32
	CPUConfigDigest string
	Resources       ResourceVector
}

type Pool struct {
	WorkerGroupID uuid.UUID
	VMPlatformID  string
	PerVM         ResourceVector
	CPUShapes     []vmplatform.CPUShape
}

func CanRestore(requirements RestoreRequirements, pool Pool) bool {
	if requirements.WorkerGroupID == uuid.Nil() || pool.WorkerGroupID == uuid.Nil() ||
		pool.WorkerGroupID != requirements.WorkerGroupID ||
		requirements.VMPlatformID == "" || pool.VMPlatformID != requirements.VMPlatformID ||
		requirements.VCPUCount <= 0 || requirements.CPUConfigDigest == "" ||
		!fitsPhysical(pool.PerVM, requirements.Resources) {
		return false
	}
	for _, shape := range pool.CPUShapes {
		if shape.VCPUCount == requirements.VCPUCount {
			return shape.CPUConfigDigest == requirements.CPUConfigDigest
		}
	}
	return false
}

type poolPlan struct {
	id       pgtype.UUID
	max      int32
	pool     Pool
	template bin
	bins     []bin
	result   PoolPlan
}

func Plan(ctx context.Context, store PlanStore, workerGroupID uuid.UUID, request PlanRequest, now time.Time) (PlanResponse, error) {
	if len(request.Pools) == 0 || len(request.Pools) > maximumPlanningPools {
		return PlanResponse{}, fmt.Errorf("%w: pools must contain between 1 and %d entries", ErrInvalidPlanRequest, maximumPlanningPools)
	}
	limits := make(map[[16]byte]int32, len(request.Pools))
	poolIDs := make([]pgtype.UUID, 0, len(request.Pools))
	for index, requested := range request.Pools {
		id, err := ids.Parse(requested.PoolID)
		if err != nil {
			return PlanResponse{}, fmt.Errorf("%w: pools[%d].pool_id must be a canonical UUIDv7", ErrInvalidPlanRequest, index)
		}
		if requested.MaxAdditionalWorkers < 0 || requested.MaxAdditionalWorkers > maximumAdditionalWorkers {
			return PlanResponse{}, fmt.Errorf("%w: pools[%d].max_additional_workers must be between 0 and %d", ErrInvalidPlanRequest, index, maximumAdditionalWorkers)
		}
		key := [16]byte(id)
		if _, duplicate := limits[key]; duplicate {
			return PlanResponse{}, fmt.Errorf("%w: pools[%d].pool_id is duplicated", ErrInvalidPlanRequest, index)
		}
		limits[key] = requested.MaxAdditionalWorkers
		poolIDs = append(poolIDs, pgvalue.UUID(id))
	}
	group, err := store.GetWorkerGroup(ctx, pgvalue.UUID(workerGroupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanResponse{}, ErrGroupNotFound
	}
	if err != nil {
		return PlanResponse{}, err
	}
	rows, err := store.ListCapacityWorkerPools(ctx, db.ListCapacityWorkerPoolsParams{
		WorkerGroupID: group.ID, WorkerPoolIDs: poolIDs,
	})
	if err != nil {
		return PlanResponse{}, fmt.Errorf("list capacity Worker pools: %w", err)
	}
	if len(rows) != len(poolIDs) {
		return PlanResponse{}, fmt.Errorf("%w: every requested pool must be active in the Worker Group", ErrInvalidPlanRequest)
	}
	response := PlanResponse{
		WorkerGroupID: pgvalue.UUIDString(group.ID), WorkerGroupName: group.Name, RegionID: group.RegionID,
		GroupStatus: WorkerGroupStatus(group.Status),
		Complete:    true, ComputedAt: now.UTC(), Pools: make([]PoolPlan, 0, len(rows)),
		UnmatchedDemand: []Incompatibility{},
	}
	plans := make([]poolPlan, 0, len(rows))
	planByID := make(map[[16]byte]*poolPlan, len(rows))
	for _, row := range rows {
		plan, err := capacityPoolPlan(row, limits[row.ID.Bytes])
		if err != nil {
			return PlanResponse{}, fmt.Errorf("load capacity Worker pool: %w", err)
		}
		plans = append(plans, plan)
	}
	sort.Slice(plans, func(i, j int) bool { return bytes.Compare(plans[i].id.Bytes[:], plans[j].id.Bytes[:]) < 0 })
	for index := range plans {
		planByID[plans[index].id.Bytes] = &plans[index]
	}
	if group.Status != string(WorkerGroupStatusActive) {
		for index := range plans {
			response.Pools = append(response.Pools, plans[index].result)
		}
		return response, nil
	}

	items, accountedPoolIDs, complete, err := discoverItems(ctx, store, group)
	if err != nil {
		return PlanResponse{}, err
	}
	response.Complete = complete
	for poolID := range accountedPoolIDs {
		if plan := planByID[poolID]; plan != nil {
			plan.result.ScaleInBlocked = true
		}
	}
	bins, binsComplete, err := currentBins(ctx, store, pgvalue.MustUUIDValue(group.ID))
	if err != nil {
		return PlanResponse{}, err
	}
	response.Complete = response.Complete && binsComplete
	reasons := map[string]int64{}
	if !binsComplete {
		for index := range plans {
			plans[index].result.Complete = false
			response.Pools = append(response.Pools, plans[index].result)
		}
		return response, nil
	}
	for index := range items {
		if items[index].reason != "" || items[index].restore != nil {
			continue
		}
		items[index].targetPoolID = group.PrimaryPoolID
		if !items[index].targetPoolID.Valid {
			items[index].reason = reasonPrimaryPool
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if left.resources.CPUMillis != right.resources.CPUMillis {
			return left.resources.CPUMillis > right.resources.CPUMillis
		}
		if left.resources.MemoryBytes != right.resources.MemoryBytes {
			return left.resources.MemoryBytes > right.resources.MemoryBytes
		}
		// Fresh work has exactly one eligible pool; restores may use secondary capacity.
		if (left.restore == nil) != (right.restore == nil) {
			return left.restore == nil
		}
		if left.resources.GuestEphemeralDiskBytes != right.resources.GuestEphemeralDiskBytes {
			return left.resources.GuestEphemeralDiskBytes > right.resources.GuestEphemeralDiskBytes
		}
		if left.role != right.role {
			return left.role < right.role
		}
		return left.key < right.key
	})

	budgets := make(map[[16]byte]environmentBudget)
	for _, candidate := range items {
		budget, exists := budgets[candidate.environment]
		if !exists {
			budget = candidate.budget
		}
		residents := int64(0)
		if candidate.computer {
			residents = 1
		}
		if candidate.resources.CPUMillis > budget.cpu || candidate.resources.MemoryBytes > budget.memory || residents > budget.residents {
			continue
		}
		consume := func() {
			budget.cpu -= candidate.resources.CPUMillis
			budget.memory -= candidate.resources.MemoryBytes
			budget.residents -= residents
			budgets[candidate.environment] = budget
		}
		if candidate.reason != "" {
			reasons[candidate.reason]++
			continue
		}
		placed := false
		for index := range bins {
			if candidateMatchesBin(candidate, bins[index]) && place(&bins[index], candidate) {
				if plan, ok := planByID[bins[index].workerPoolID.Bytes]; ok {
					plan.result.CompatibleQueuedItems++
				}
				placed = true
				break
			}
		}
		if placed {
			consume()
			continue
		}
		var compatiblePoolIndexes [maximumPlanningPools]int
		compatiblePoolCount := 0
		for index := range plans {
			plan := &plans[index]
			if candidate.restore != nil {
				if !CanRestore(*candidate.restore, plan.pool) {
					continue
				}
			} else if !candidate.targetPoolID.Valid || candidate.targetPoolID != plan.id {
				continue
			}
			if incompatibility(candidate, plan.template) != "" {
				continue
			}
			compatiblePoolIndexes[compatiblePoolCount] = index
			compatiblePoolCount++
		}
		if compatiblePoolCount == 0 {
			if candidate.restore != nil {
				reasons[reasonRuntimeCompatibility]++
			} else if _, exists := planByID[candidate.targetPoolID.Bytes]; !exists {
				reasons[reasonProviderPool]++
			} else {
				reasons[reasonPerInstanceResources]++
			}
			continue
		}
		assigned := false
		for _, planIndex := range compatiblePoolIndexes[:compatiblePoolCount] {
			plan := &plans[planIndex]
			for index := range plan.bins {
				if place(&plan.bins[index], candidate) {
					plan.result.CompatibleQueuedItems++
					if candidate.restore != nil {
						plan.result.ScaleInBlocked = true
					}
					assigned = true
					break
				}
			}
			if assigned {
				break
			}
		}
		if assigned {
			consume()
			continue
		}
		// Reuse compatible capacity first. For new Workers, prefer the current
		// primary while retaining deterministic secondary choices when needed.
		sort.SliceStable(compatiblePoolIndexes[:compatiblePoolCount], func(i, j int) bool {
			return plans[compatiblePoolIndexes[i]].id == group.PrimaryPoolID &&
				plans[compatiblePoolIndexes[j]].id != group.PrimaryPoolID
		})
		for _, planIndex := range compatiblePoolIndexes[:compatiblePoolCount] {
			plan := &plans[planIndex]
			if int32(len(plan.bins)) >= plan.max {
				plan.result.Saturated = true
				continue
			}
			fresh := plan.template
			if !place(&fresh, candidate) {
				continue
			}
			plan.bins = append(plan.bins, fresh)
			plan.result.RecommendedAdditionalWorkers++
			plan.result.CompatibleQueuedItems++
			if candidate.restore != nil {
				plan.result.ScaleInBlocked = true
			}
			assigned = true
			break
		}
		if !assigned {
			reasons[reasonProviderSaturated]++
		} else {
			consume()
		}
	}
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Strings(keys)
	for _, reason := range keys {
		response.UnmatchedDemand = append(response.UnmatchedDemand, Incompatibility{Reason: reason, Count: reasons[reason]})
	}
	for index := range plans {
		plans[index].result.Complete = response.Complete
		response.Pools = append(response.Pools, plans[index].result)
	}
	return response, nil
}

func capacityPoolPlan(row db.ListCapacityWorkerPoolsRow, max int32) (poolPlan, error) {
	if !row.VMPlatformID.Valid ||
		!row.CapacityCPUMillis.Valid || !row.CapacityMemoryBytes.Valid ||
		!row.CapacityGuestEphemeralDiskBytes.Valid || !row.PerVMCPUMillis.Valid ||
		!row.PerVMMemoryBytes.Valid || !row.PerVMGuestEphemeralDiskBytes.Valid ||
		!row.MaxVMSlots.Valid ||
		len(row.CPUShapeVCPUCounts) != len(row.CPUShapeConfigDigests) {
		return poolPlan{}, errors.New("active Worker pool has an incomplete template")
	}
	shapes := make([]vmplatform.CPUShape, len(row.CPUShapeVCPUCounts))
	for index := range row.CPUShapeVCPUCounts {
		shapes[index] = vmplatform.CPUShape{
			VCPUCount: row.CPUShapeVCPUCounts[index], CPUConfigDigest: row.CPUShapeConfigDigests[index],
		}
	}
	resources := ResourceVector{
		CPUMillis: row.CapacityCPUMillis.Int64, MemoryBytes: row.CapacityMemoryBytes.Int64,
		GuestEphemeralDiskBytes: row.CapacityGuestEphemeralDiskBytes.Int64,
		VMSlots:                 int64(row.MaxVMSlots.Int32),
	}
	perVM := ResourceVector{
		CPUMillis: row.PerVMCPUMillis.Int64, MemoryBytes: row.PerVMMemoryBytes.Int64,
		GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes.Int64,
	}
	workerGroupID := pgvalue.MustUUIDValue(row.WorkerGroupID)
	template := bin{
		workerGroupID: workerGroupID, workerPoolID: row.ID,
		resources: resources, instanceStarts: resources.VMSlots,
		supportsRun: true,
		runtimeArch: "x86_64", runtimeContract: vmplatform.Contract,
		vmPlatformID: row.VMPlatformID.String,
		perVM:        perVM, cpuShapes: shapes,
	}
	return poolPlan{
		id: row.ID, max: max,
		pool: Pool{
			WorkerGroupID: workerGroupID, VMPlatformID: row.VMPlatformID.String,
			PerVM: perVM, CPUShapes: shapes,
		},
		template: template,
		result: PoolPlan{
			PoolID: uuid.UUID(row.ID.Bytes).String(), PoolName: row.Name,
			RegisteringWorkers: row.RegisteringWorkers, ActiveWorkers: row.ActiveWorkers,
			Complete: true,
		},
	}, nil
}

func candidateMatchesBin(candidate item, target bin) bool {
	if candidate.restore != nil {
		return CanRestore(*candidate.restore, Pool{
			WorkerGroupID: target.workerGroupID, VMPlatformID: target.vmPlatformID,
			PerVM: target.perVM, CPUShapes: target.cpuShapes,
		})
	}
	return candidate.targetPoolID.Valid && candidate.targetPoolID == target.workerPoolID
}

func discoverItems(ctx context.Context, store PlanStore, group db.WorkerGroup) ([]item, map[[16]byte]struct{}, bool, error) {
	rows, err := store.ListAllocationPlanningDemand(ctx, db.ListAllocationPlanningDemandParams{RegionID: group.RegionID, WorkerGroupID: group.ID, RowLimit: maximumPlanningCandidates + 1})
	if err != nil {
		return nil, nil, false, fmt.Errorf("list allocation capacity demand: %w", err)
	}
	complete := len(rows) <= int(maximumPlanningCandidates)
	if !complete {
		rows = rows[:maximumPlanningCandidates]
	}
	result := make([]item, 0, len(rows))
	charged, err := store.ListAllocationPlanningChargedPools(ctx, group.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("list allocation charged pools: %w", err)
	}
	accounted := make(map[[16]byte]struct{})
	for _, id := range charged {
		accounted[id.Bytes] = struct{}{}
	}
	for _, row := range rows {
		result = append(result, allocationItem(row))
	}
	return result, accounted, complete, nil
}

func allocationItem(row db.ListAllocationPlanningDemandRow) item {
	result := item{environment: row.EnvironmentID.Bytes, computer: row.Kind == "computer", budget: environmentBudget{cpu: row.RemainingCpuMillis, memory: row.RemainingMemoryBytes, residents: row.RemainingResidents}, role: "run", key: fmt.Sprintf("%s:%x:%x", row.Kind, row.EnvironmentID.Bytes, row.OwnerID.Bytes)}
	var declared definition.ResourcesManifest
	if json.Unmarshal(row.Resources, &declared) != nil || definition.ValidateResourcesManifest(declared) != nil || declared.MemoryMiB > math.MaxInt64/mebibyte {
		result.reason = reasonInvalidWorkload
		return result
	}
	cpu, err := vm.ReservedCPUMillis(declared.MilliCPU)
	if err != nil {
		result.reason = reasonInvalidWorkload
		return result
	}
	result.resources = ResourceVector{CPUMillis: cpu, MemoryBytes: declared.MemoryMiB * mebibyte, VMSlots: 1}
	if row.RequiredVMPlatformID != "" {
		if !row.RequiredWorkerGroupID.Valid || row.RequiredVMVCPUCount <= 0 || row.RequiredCPUConfigDigest == "" || row.RequiredCPUMillis <= 0 || row.RequiredMemoryBytes <= 0 || row.RequiredScratchBytes <= 0 {
			result.reason = reasonInvalidWorkload
			return result
		}
		result.resources = ResourceVector{CPUMillis: row.RequiredCPUMillis, MemoryBytes: row.RequiredMemoryBytes, GuestEphemeralDiskBytes: row.RequiredScratchBytes, VMSlots: 1}
		result.restore = &RestoreRequirements{WorkerGroupID: pgvalue.MustUUIDValue(row.RequiredWorkerGroupID), VMPlatformID: row.RequiredVMPlatformID, VCPUCount: row.RequiredVMVCPUCount, CPUConfigDigest: row.RequiredCPUConfigDigest, Resources: result.resources}
		result.vmPlatformID = row.RequiredVMPlatformID
	}
	return result
}

func currentBins(ctx context.Context, store PlanStore, workerGroupID uuid.UUID) ([]bin, bool, error) {
	rows, err := store.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID: pgvalue.UUID(workerGroupID), ObservationFreshnessSeconds: ObservationFreshnessSeconds,
		RowLimit: maximumPlanningWorkers + 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("list current Worker capacity bins: %w", err)
	}
	complete := len(rows) <= maximumPlanningWorkers
	if !complete {
		rows = rows[:maximumPlanningWorkers]
	}
	result := make([]bin, 0, len(rows))
	for _, row := range rows {
		result = append(result, binFromRow(row))
	}
	return result, complete, nil
}

func binFromRow(row db.ListWorkerCapacityBinsRow) bin {
	result := bin{
		workerGroupID: pgvalue.MustUUIDValue(row.WorkerGroupID),
		workerPoolID:  row.WorkerPoolID,
		resources: ResourceVector{
			CPUMillis: row.AvailableCPUMillis, MemoryBytes: row.AvailableMemoryBytes,
			GuestEphemeralDiskBytes: row.AvailableGuestEphemeralDiskBytes,
			VMSlots:                 row.AvailableVMSlots,
		},
		instanceStarts: row.AvailableInstanceStarts, supportsRun: true,
		runtimeArch:     row.Arch,
		runtimeContract: row.Contract, vmPlatformID: row.VMPlatformID.String,
		runPaused:      row.RunPausedReason.Valid,
		instancePaused: row.VMPausedReason.Valid,
		perVM: ResourceVector{
			CPUMillis: row.PerVMCPUMillis, MemoryBytes: row.PerVMMemoryBytes,
			GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes,
		},
	}
	if len(row.CPUShapeVCPUCounts) == len(row.CPUShapeConfigDigests) {
		result.cpuShapes = make([]vmplatform.CPUShape, len(row.CPUShapeVCPUCounts))
		for index := range row.CPUShapeVCPUCounts {
			result.cpuShapes[index] = vmplatform.CPUShape{
				VCPUCount: row.CPUShapeVCPUCounts[index], CPUConfigDigest: row.CPUShapeConfigDigests[index],
			}
		}
	}
	return result
}

func incompatibility(candidate item, target bin) string {
	if candidate.restore == nil {
		qualified := false
		for _, shape := range target.cpuShapes {
			if int64(shape.VCPUCount) == candidate.resources.CPUMillis/1000 && shape.CPUConfigDigest != "" {
				qualified = true
				break
			}
		}
		if !qualified {
			return reasonRuntimeCompatibility
		}
	}
	if candidate.role == "run" && !target.supportsRun {
		return reasonRunRole
	}
	if target.runtimeArch != "x86_64" || target.runtimeContract != vmplatform.Contract {
		return reasonRuntimeCompatibility
	}
	if candidate.role == "run" && candidate.vmPlatformID != "" && candidate.vmPlatformID != target.vmPlatformID {
		return reasonRuntimeCompatibility
	}
	if !fitsResources(target.resources, candidate.resources) {
		return reasonPerInstanceResources
	}
	if candidate.role == "run" && !fitsPhysical(target.perVM, candidate.resources) {
		return reasonPerInstanceResources
	}
	return ""
}

func place(target *bin, candidate item) bool {
	if candidate.restore == nil {
		candidate.resources.GuestEphemeralDiskBytes = target.perVM.GuestEphemeralDiskBytes
	}
	if incompatibility(candidate, *target) != "" {
		return false
	}
	if candidate.role == "run" && (target.runPaused || target.instancePaused || target.instanceStarts <= 0) {
		return false
	}
	target.resources.CPUMillis -= candidate.resources.CPUMillis
	target.resources.MemoryBytes -= candidate.resources.MemoryBytes
	target.resources.GuestEphemeralDiskBytes -= candidate.resources.GuestEphemeralDiskBytes
	if candidate.role == "run" {
		target.resources.VMSlots--
		target.instanceStarts--
	}
	return true
}

func fitsResources(available, required ResourceVector) bool {
	return available.CPUMillis >= required.CPUMillis &&
		available.MemoryBytes >= required.MemoryBytes &&
		available.GuestEphemeralDiskBytes >= required.GuestEphemeralDiskBytes &&
		available.VMSlots >= required.VMSlots
}

func fitsPhysical(available, required ResourceVector) bool {
	return available.CPUMillis >= required.CPUMillis &&
		available.MemoryBytes >= required.MemoryBytes &&
		available.GuestEphemeralDiskBytes >= required.GuestEphemeralDiskBytes
}
