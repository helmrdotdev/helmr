package capacity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	planningScopePageSize     = int32(128)
	maximumPlanningScopes     = int32(5000)
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
	reasonQueueConcurrency     = "queue_concurrency"
	reasonRuntimeCompatibility = "runtime_compatibility"
	reasonInvalidWorkload      = "invalid_workload_requirements"
	reasonPrimaryPool          = "primary_pool_unavailable"
	reasonProviderPool         = "provider_pool_unavailable"
	reasonProviderSaturated    = "provider_capacity_saturated"
)

type Store interface {
	GetWorkerGroup(context.Context, string) (db.WorkerGroup, error)
	ListCapacityWorkerPools(context.Context, db.ListCapacityWorkerPoolsParams) ([]db.ListCapacityWorkerPoolsRow, error)
	ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error)
	ListQueuedRunEligibleScopes(context.Context, db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error)
	ListQueuedRunPlanningUsage(context.Context, db.ListQueuedRunPlanningUsageParams) ([]db.ListQueuedRunPlanningUsageRow, error)
	ListQueuedRunPlanningCandidatesForScopes(context.Context, db.ListQueuedRunPlanningCandidatesForScopesParams) ([]db.ListQueuedRunPlanningCandidatesForScopesRow, error)
	ListPendingWorkspaceExecCapacityCandidates(context.Context, db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error)
}

type item struct {
	role              string
	resources         ResourceVector
	targetPoolID      pgtype.UUID
	restore           *RestoreRequirements
	runtimeIdentityID string
	substrateFormat   string
	substrateContract string
	reason            string
	key               string
}

type bin struct {
	workerGroupID     string
	workerPoolID      pgtype.UUID
	resources         ResourceVector
	runConsumers      int64
	runtimeStarts     int64
	supportsRun       bool
	runtimeArch       string
	runtimeContract   string
	runtimeIdentityID string
	substrateFormat   string
	substrateContract string
	runPaused         bool
	runtimePaused     bool
	perVM             ResourceVector
	cpuShapes         []runtimeid.CPUShape
}

type RestoreRequirements struct {
	WorkerGroupID     string
	RuntimeIdentityID string
	VCPUCount         int32
	CPUConfigDigest   string
	SubstrateFormat   string
	SubstrateContract string
	Resources         ResourceVector
}

type Pool struct {
	WorkerGroupID     string
	RuntimeIdentityID string
	SubstrateFormat   string
	SubstrateContract string
	PerVM             ResourceVector
	CPUShapes         []runtimeid.CPUShape
}

func CanRestore(requirements RestoreRequirements, pool Pool) bool {
	if requirements.WorkerGroupID == "" || pool.WorkerGroupID != requirements.WorkerGroupID ||
		requirements.RuntimeIdentityID == "" || pool.RuntimeIdentityID != requirements.RuntimeIdentityID ||
		requirements.VCPUCount <= 0 || requirements.CPUConfigDigest == "" ||
		pool.SubstrateFormat != requirements.SubstrateFormat ||
		pool.SubstrateContract != requirements.SubstrateContract ||
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

func Plan(ctx context.Context, store Store, workerGroupID string, request PlanRequest, now time.Time) (PlanResponse, error) {
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
	group, err := store.GetWorkerGroup(ctx, workerGroupID)
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
		WorkerGroupID: group.ID, WorkerGroupName: group.Name, RegionID: group.RegionID,
		GroupStatus: WorkerGroupStatus(group.State),
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
	if group.State != string(WorkerGroupStatusActive) {
		for index := range plans {
			response.Pools = append(response.Pools, plans[index].result)
		}
		return response, nil
	}

	items, accountedPoolIDs, complete, err := discoverItems(ctx, store, group, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return PlanResponse{}, err
	}
	response.Complete = complete
	for poolID := range accountedPoolIDs {
		if plan := planByID[poolID]; plan != nil {
			plan.result.ScaleInBlocked = true
		}
	}
	bins, binsComplete, err := currentBins(ctx, store, group.ID)
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
		if left.resources.GuestEphemeralDiskBytes != right.resources.GuestEphemeralDiskBytes {
			return left.resources.GuestEphemeralDiskBytes > right.resources.GuestEphemeralDiskBytes
		}
		if left.role != right.role {
			return left.role < right.role
		}
		return left.key < right.key
	})

	for _, candidate := range items {
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
			continue
		}
		compatiblePools := requestedPoolsForCandidate(plans, candidate)
		if len(compatiblePools) == 0 {
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
		for _, plan := range compatiblePools {
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
			continue
		}
		for _, plan := range compatiblePools {
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
	if !row.RuntimeIdentityID.Valid || !row.SubstrateFormat.Valid || !row.SubstrateContract.Valid ||
		!row.CapacityCPUMillis.Valid || !row.CapacityMemoryBytes.Valid ||
		!row.CapacityGuestEphemeralDiskBytes.Valid || !row.PerVMCPUMillis.Valid ||
		!row.PerVMMemoryBytes.Valid || !row.PerVMGuestEphemeralDiskBytes.Valid ||
		!row.MaxVMSlots.Valid ||
		len(row.CPUShapeVCPUCounts) != len(row.CPUShapeConfigDigests) {
		return poolPlan{}, errors.New("active Worker pool has an incomplete template")
	}
	shapes := make([]runtimeid.CPUShape, len(row.CPUShapeVCPUCounts))
	for index := range row.CPUShapeVCPUCounts {
		shapes[index] = runtimeid.CPUShape{
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
	template := bin{
		workerGroupID: row.WorkerGroupID, workerPoolID: row.ID,
		resources: resources, runConsumers: resources.VMSlots, runtimeStarts: resources.VMSlots,
		supportsRun: true,
		runtimeArch: "x86_64", runtimeContract: runtimeid.Contract,
		runtimeIdentityID: row.RuntimeIdentityID.String,
		substrateFormat:   row.SubstrateFormat.String, substrateContract: row.SubstrateContract.String,
		perVM: perVM, cpuShapes: shapes,
	}
	return poolPlan{
		id: row.ID, max: max,
		pool: Pool{
			WorkerGroupID: row.WorkerGroupID, RuntimeIdentityID: row.RuntimeIdentityID.String,
			SubstrateFormat: row.SubstrateFormat.String, SubstrateContract: row.SubstrateContract.String,
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
			WorkerGroupID: target.workerGroupID, RuntimeIdentityID: target.runtimeIdentityID,
			SubstrateFormat: target.substrateFormat, SubstrateContract: target.substrateContract,
			PerVM: target.perVM, CPUShapes: target.cpuShapes,
		})
	}
	return candidate.targetPoolID.Valid && candidate.targetPoolID == target.workerPoolID
}

func requestedPoolsForCandidate(plans []poolPlan, candidate item) []*poolPlan {
	result := make([]*poolPlan, 0, len(plans))
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
		result = append(result, plan)
	}
	return result
}

func discoverItems(ctx context.Context, store Store, group db.WorkerGroup, scanSeed string) ([]item, map[[16]byte]struct{}, bool, error) {
	result := make([]item, 0)
	accountedPoolIDs := make(map[[16]byte]struct{})
	complete := true
	{
		remaining := maximumPlanningCandidates
		var after db.ListQueuedRunEligibleScopesRow
		var visited int32
		for remaining > 0 && visited < maximumPlanningScopes {
			pageLimit := min(planningScopePageSize, maximumPlanningScopes-visited)
			scopes, err := store.ListQueuedRunEligibleScopes(ctx, db.ListQueuedRunEligibleScopesParams{
				AfterSortKey: after.SortKey, AfterOrgID: after.OrgID, AfterProjectID: after.ProjectID,
				AfterEnvironmentID: after.EnvironmentID, AfterRegionID: after.RegionID,
				AfterConcurrencyKey: after.ConcurrencyKey, AfterQueueName: after.QueueName,
				RowLimit: pageLimit, ScanSeed: scanSeed, RegionFilter: group.RegionID,
			})
			if err != nil {
				return nil, nil, false, fmt.Errorf("list capacity planning run scopes: %w", err)
			}
			if len(scopes) == 0 {
				break
			}
			usage, err := store.ListQueuedRunPlanningUsage(ctx, planningUsageParams(scopes))
			if err != nil {
				return nil, nil, false, fmt.Errorf("list capacity planning run usage: %w", err)
			}
			if len(usage) != len(scopes) {
				return nil, nil, false, fmt.Errorf("list capacity planning run usage: got %d rows for %d scopes", len(usage), len(scopes))
			}
			visited += int32(len(scopes))
			for index, scope := range scopes {
				if usage[index].ScopeOrdinal != int64(index+1) {
					return nil, nil, false, fmt.Errorf("list capacity planning run usage: ordinal %d at index %d", usage[index].ScopeOrdinal, index)
				}
				if remaining <= 0 {
					complete = false
					break
				}
				rows, err := store.ListQueuedRunPlanningCandidatesForScopes(ctx, planningCandidateParams(scope, remaining+1))
				if err != nil {
					return nil, nil, false, fmt.Errorf("list capacity planning run candidates: %w", err)
				}
				if len(rows) > int(remaining) {
					complete = false
					rows = rows[:remaining]
				}
				admission := queueAdmissionStateFromUsage(usage[index])
				for _, row := range rows {
					candidate := runItem(row)
					if candidate.reason == "" && !admission.admit(row.QueueConcurrencyLimit) {
						candidate.reason = reasonQueueConcurrency
					}
					result = append(result, candidate)
					remaining--
				}
			}
			last := scopes[len(scopes)-1]
			after = last
			if len(scopes) < int(pageLimit) {
				break
			}
		}
		if visited >= maximumPlanningScopes {
			complete = false
		}
	}
	execs, err := store.ListPendingWorkspaceExecCapacityCandidates(ctx, db.ListPendingWorkspaceExecCapacityCandidatesParams{
		RegionID: group.RegionID,
		RowLimit: maximumPlanningCandidates + 1,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("list capacity planning Workspace Exec candidates: %w", err)
	}
	if len(execs) > int(maximumPlanningCandidates) {
		complete = false
		execs = execs[:maximumPlanningCandidates]
	}
	for _, row := range execs {
		if len(row.AccountedPoolIds) > 0 {
			for _, poolID := range row.AccountedPoolIds {
				accountedPoolIDs[poolID.Bytes] = struct{}{}
			}
			continue
		}
		result = append(result, workspaceExecItem(row))
	}
	return result, accountedPoolIDs, complete, nil
}

type queueAdmissionState struct {
	activeRuns, activeLimit     int64
	preparedRuns, preparedLimit int64
	admitted                    int64
}

func planningUsageParams(scopes []db.ListQueuedRunEligibleScopesRow) db.ListQueuedRunPlanningUsageParams {
	params := db.ListQueuedRunPlanningUsageParams{
		EnvironmentIds:  make([]pgtype.UUID, 0, len(scopes)),
		ConcurrencyKeys: make([]string, 0, len(scopes)),
		QueueNames:      make([]string, 0, len(scopes)),
	}
	for _, scope := range scopes {
		params.EnvironmentIds = append(params.EnvironmentIds, scope.EnvironmentID)
		params.ConcurrencyKeys = append(params.ConcurrencyKeys, scope.ConcurrencyKey)
		params.QueueNames = append(params.QueueNames, scope.QueueName)
	}
	return params
}

func planningCandidateParams(scope db.ListQueuedRunEligibleScopesRow, limit int32) db.ListQueuedRunPlanningCandidatesForScopesParams {
	return db.ListQueuedRunPlanningCandidatesForScopesParams{
		PerScopeLimit: limit,
		OrgIds:        []pgtype.UUID{scope.OrgID}, ProjectIds: []pgtype.UUID{scope.ProjectID},
		EnvironmentIds: []pgtype.UUID{scope.EnvironmentID}, RegionIds: []string{scope.RegionID},
		ConcurrencyKeys: []string{scope.ConcurrencyKey}, QueueNames: []string{scope.QueueName},
	}
}

func queueAdmissionStateFromUsage(usage db.ListQueuedRunPlanningUsageRow) queueAdmissionState {
	return queueAdmissionState{
		activeRuns: usage.ActiveRuns, activeLimit: usage.ActiveLimit,
		preparedRuns: usage.PreparedRuns, preparedLimit: usage.PreparedLimit,
	}
}

func (s *queueAdmissionState) admit(candidateLimit pgtype.Int8) bool {
	if exceedsQueueLimit(s.activeRuns+s.admitted, candidateLimit, s.activeLimit) ||
		exceedsQueueLimit(s.preparedRuns+s.admitted, candidateLimit, s.preparedLimit) {
		return false
	}
	s.admitted++
	return true
}

func exceedsQueueLimit(used int64, candidateLimit pgtype.Int8, pinnedLimit int64) bool {
	limit := candidateLimit
	if pinnedLimit > 0 && (!limit.Valid || pinnedLimit < limit.Int64) {
		limit = pgtype.Int8{Int64: pinnedLimit, Valid: true}
	}
	return limit.Valid && used >= limit.Int64
}

func runItem(row db.ListQueuedRunPlanningCandidatesForScopesRow) item {
	result := freshExecutionItem(fmt.Sprintf("%x", row.RunID.Bytes), row.WorkspaceManifestVersion, row.WorkspaceManifest)
	if result.reason != "" {
		return result
	}
	resources := result.resources
	if row.RequiredRuntimeIdentityID != "" {
		if row.RequiredWorkerGroupID == "" || row.RequiredVMVCPUCount <= 0 ||
			row.RequiredCPUConfigDigest == "" || row.RequiredCPUMillis <= 0 ||
			row.RequiredMemoryBytes <= 0 || row.RequiredGuestEphemeralDiskBytes <= 0 ||
			row.RequiredSubstrateFormat == "" || row.RequiredSubstrateContract == "" {
			result.reason = reasonInvalidWorkload
			return result
		}
		resources = ResourceVector{
			CPUMillis: row.RequiredCPUMillis, MemoryBytes: row.RequiredMemoryBytes,
			GuestEphemeralDiskBytes: row.RequiredGuestEphemeralDiskBytes, VMSlots: 1,
		}
		result.restore = &RestoreRequirements{
			WorkerGroupID: row.RequiredWorkerGroupID, RuntimeIdentityID: row.RequiredRuntimeIdentityID,
			VCPUCount: row.RequiredVMVCPUCount, CPUConfigDigest: row.RequiredCPUConfigDigest,
			SubstrateFormat: row.RequiredSubstrateFormat, SubstrateContract: row.RequiredSubstrateContract,
			Resources: resources,
		}
	}
	result.resources = resources
	result.runtimeIdentityID = row.RequiredRuntimeIdentityID
	result.substrateFormat = row.RequiredSubstrateFormat
	result.substrateContract = row.RequiredSubstrateContract
	return result
}

func workspaceExecItem(row db.ListPendingWorkspaceExecCapacityCandidatesRow) item {
	return freshExecutionItem(fmt.Sprintf("workspace-exec:%x", row.ProcessID.Bytes), row.WorkspaceManifestVersion, row.WorkspaceManifest)
}

func freshExecutionItem(key string, manifestVersion int32, manifestJSON []byte) item {
	result := item{role: "run", key: key}
	manifest, err := deployment.ParseSandboxManifest(manifestVersion, manifestJSON)
	if err != nil {
		result.reason = reasonInvalidWorkload
		return result
	}
	if manifest.Resources.MilliCPU <= 0 || manifest.Resources.MemoryMiB <= 0 ||
		manifest.Resources.MemoryMiB > math.MaxInt64/mebibyte {
		result.reason = reasonInvalidWorkload
		return result
	}
	resources := ResourceVector{
		CPUMillis: manifest.Resources.MilliCPU, MemoryBytes: manifest.Resources.MemoryMiB * mebibyte,
		GuestEphemeralDiskBytes: compute.WorkspaceGuestEphemeralDiskMiB * mebibyte,
		VMSlots:                 1,
	}
	result.resources = resources
	return result
}

func currentBins(ctx context.Context, store Store, workerGroupID string) ([]bin, bool, error) {
	rows, err := store.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID: workerGroupID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
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
		workerGroupID: row.WorkerGroupID,
		workerPoolID:  row.WorkerPoolID,
		resources: ResourceVector{
			CPUMillis: row.AvailableCPUMillis, MemoryBytes: row.AvailableMemoryBytes,
			GuestEphemeralDiskBytes: row.AvailableGuestEphemeralDiskBytes,
			VMSlots:                 row.AvailableVMSlots,
		},
		runConsumers: row.AvailableRunConsumers, runtimeStarts: row.AvailableRuntimeStarts, supportsRun: true,
		runtimeArch:     row.RuntimeArch,
		runtimeContract: row.VMRuntimeContract, runtimeIdentityID: row.RuntimeIdentityID.String,
		substrateFormat: row.SubstrateFormat, substrateContract: row.SubstrateContract,
		runPaused:     row.RunPausedReason.Valid,
		runtimePaused: row.RuntimePausedReason.Valid,
		perVM: ResourceVector{
			CPUMillis: row.PerVMCPUMillis, MemoryBytes: row.PerVMMemoryBytes,
			GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes,
		},
	}
	if len(row.CPUShapeVCPUCounts) == len(row.CPUShapeConfigDigests) {
		result.cpuShapes = make([]runtimeid.CPUShape, len(row.CPUShapeVCPUCounts))
		for index := range row.CPUShapeVCPUCounts {
			result.cpuShapes[index] = runtimeid.CPUShape{
				VCPUCount: row.CPUShapeVCPUCounts[index], CPUConfigDigest: row.CPUShapeConfigDigests[index],
			}
		}
	}
	return result
}

type RunRequirements struct {
	Resources         ResourceVector
	Architecture      string
	WorkerGroupID     string
	RuntimeIdentityID string
	VCPUCount         int32
	CPUConfigDigest   string
	SubstrateFormat   string
	SubstrateContract string
}

func SelectRunWorker(rows []db.ListWorkerCapacityBinsRow, request RunRequirements) (db.ListWorkerCapacityBinsRow, bool) {
	for _, row := range rows {
		target, compatible := hardCompatibleRunWorker(row, request)
		if !compatible {
			continue
		}
		if !fitsResources(target.resources, request.Resources) || target.runConsumers <= 0 || target.runtimeStarts <= 0 {
			continue
		}
		return row, true
	}
	return db.ListWorkerCapacityBinsRow{}, false
}

// RunWorkerCapacityPressureCandidates returns only Workers that can safely
// host the Run after an existing idle Runtime is physically reclaimed. Dynamic
// free-capacity counters are deliberately ignored; every identity, profile,
// pause, architecture, and per-VM ceiling remains part of the filter.
func RunWorkerCapacityPressureCandidates(rows []db.ListWorkerCapacityBinsRow, request RunRequirements) []db.ListWorkerCapacityBinsRow {
	result := make([]db.ListWorkerCapacityBinsRow, 0, len(rows))
	for _, row := range rows {
		if _, compatible := hardCompatibleRunWorker(row, request); compatible {
			result = append(result, row)
		}
	}
	return result
}

func hardCompatibleRunWorker(row db.ListWorkerCapacityBinsRow, request RunRequirements) (bin, bool) {
	target := binFromRow(row)
	if target.runPaused || target.runtimePaused || !target.supportsRun ||
		target.runtimeArch != "x86_64" || target.runtimeContract != runtimeid.Contract ||
		!fitsPhysical(target.perVM, request.Resources) {
		return bin{}, false
	}
	if request.Architecture != "" && target.runtimeArch != request.Architecture {
		return bin{}, false
	}
	if request.RuntimeIdentityID == "" {
		if !row.PrimaryPoolID.Valid || row.WorkerPoolID != row.PrimaryPoolID {
			return bin{}, false
		}
	} else if !CanRestore(RestoreRequirements{
		WorkerGroupID: request.WorkerGroupID, RuntimeIdentityID: request.RuntimeIdentityID,
		VCPUCount: request.VCPUCount, CPUConfigDigest: request.CPUConfigDigest,
		SubstrateFormat: request.SubstrateFormat, SubstrateContract: request.SubstrateContract,
		Resources: request.Resources,
	}, Pool{
		WorkerGroupID: target.workerGroupID, RuntimeIdentityID: target.runtimeIdentityID,
		SubstrateFormat: target.substrateFormat, SubstrateContract: target.substrateContract,
		PerVM: target.perVM, CPUShapes: target.cpuShapes,
	}) {
		return bin{}, false
	}
	if request.SubstrateFormat != "" && target.substrateFormat != request.SubstrateFormat {
		return bin{}, false
	}
	if request.SubstrateContract != "" && target.substrateContract != request.SubstrateContract {
		return bin{}, false
	}
	return target, true
}

func incompatibility(candidate item, target bin) string {
	if candidate.role == "run" && !target.supportsRun {
		return reasonRunRole
	}
	if target.runtimeArch != "x86_64" || target.runtimeContract != runtimeid.Contract {
		return reasonRuntimeCompatibility
	}
	if candidate.role == "run" && ((candidate.runtimeIdentityID != "" && candidate.runtimeIdentityID != target.runtimeIdentityID) ||
		(candidate.substrateFormat != "" && candidate.substrateFormat != target.substrateFormat) ||
		(candidate.substrateContract != "" && candidate.substrateContract != target.substrateContract)) {
		return reasonRuntimeCompatibility
	}
	if !fitsResources(target.resources, candidate.resources) {
		return reasonPerInstanceResources
	}
	if candidate.role == "run" && target.runConsumers <= 0 {
		return reasonPerInstanceResources
	}
	if candidate.role == "run" && !fitsPhysical(target.perVM, candidate.resources) {
		return reasonPerInstanceResources
	}
	return ""
}

func place(target *bin, candidate item) bool {
	if incompatibility(candidate, *target) != "" {
		return false
	}
	if candidate.role == "run" && (target.runPaused || target.runtimePaused || target.runtimeStarts <= 0) {
		return false
	}
	target.resources.CPUMillis -= candidate.resources.CPUMillis
	target.resources.MemoryBytes -= candidate.resources.MemoryBytes
	target.resources.GuestEphemeralDiskBytes -= candidate.resources.GuestEphemeralDiskBytes
	if candidate.role == "run" {
		target.resources.VMSlots--
		target.runConsumers--
		target.runtimeStarts--
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
