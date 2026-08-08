package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	planningScopePageSize     = int32(128)
	maximumPlanningScopes     = int32(5000)
	maximumPlanningCandidates = int32(5000)
	maximumPlanningWorkers    = 1000
	maximumAdditionalWorkers  = int32(1000)
	mebibyte                  = int64(1024 * 1024)
)

var ErrInvalidPlanRequest = errors.New("invalid capacity plan request")

const (
	reasonBuildRole            = "worker_does_not_support_build"
	reasonRunRole              = "worker_does_not_support_run"
	reasonPerInstanceResources = "per_instance_resources"
	reasonRetainedRuntime      = "retained_runtime"
	reasonQueueConcurrency     = "queue_concurrency"
	reasonRuntimeCompatibility = "runtime_compatibility"
	reasonInvalidWorkload      = "invalid_workload_requirements"
)

type Store interface {
	GetWorkerGroup(context.Context, string) (db.WorkerGroup, error)
	ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error)
	ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error)
	ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error)
	ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error)
}

type item struct {
	role              string
	resources         capacityapi.ResourceVector
	runtimeIdentityID string
	substrateFormat   string
	substrateContract string
	reason            string
	key               string
}

type bin struct {
	resources         capacityapi.ResourceVector
	runConsumers      int64
	runtimeStarts     int64
	supportsRun       bool
	supportsBuild     bool
	runtimeArch       string
	runtimeContract   string
	runtimeIdentityID string
	substrateFormat   string
	substrateContract string
	runPaused         bool
	buildPaused       bool
	runtimePaused     bool
	perVM             capacityapi.ResourceVector
}

func Plan(ctx context.Context, store Store, workerGroupID string, request capacityapi.CapacityPlanRequest, now time.Time) (capacityapi.CapacityPlanResponse, error) {
	if err := request.Worker.Validate(); err != nil {
		return capacityapi.CapacityPlanResponse{}, fmt.Errorf("%w: validate Worker release manifest: %v", ErrInvalidPlanRequest, err)
	}
	if request.MaxAdditionalWorkers <= 0 || request.MaxAdditionalWorkers > maximumAdditionalWorkers {
		return capacityapi.CapacityPlanResponse{}, fmt.Errorf("%w: max_additional_workers must be between 1 and %d", ErrInvalidPlanRequest, maximumAdditionalWorkers)
	}
	group, err := store.GetWorkerGroup(ctx, workerGroupID)
	if err != nil {
		return capacityapi.CapacityPlanResponse{}, err
	}
	if err := validateTemplateForGroup(request.Worker, group); err != nil {
		return capacityapi.CapacityPlanResponse{}, fmt.Errorf("%w: Worker release does not satisfy Worker Group: %v", ErrInvalidPlanRequest, err)
	}
	response := capacityapi.CapacityPlanResponse{
		WorkerGroupID: group.ID, WorkerGroupName: group.Name, RegionID: group.RegionID,
		GroupStatus: capacityapi.WorkerGroupStatus(group.State), ReleaseFingerprint: request.Worker.ReleaseFingerprint,
		Complete: true, ComputedAt: now.UTC(), Incompatibilities: []capacityapi.CapacityIncompatibility{},
	}
	if group.State != string(capacityapi.WorkerGroupStatusActive) {
		return response, nil
	}

	items, complete, err := discoverItems(ctx, store, group, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return capacityapi.CapacityPlanResponse{}, err
	}
	response.Complete = complete
	bins, binsComplete, err := currentBins(ctx, store, group.ID)
	if err != nil {
		return capacityapi.CapacityPlanResponse{}, err
	}
	response.Complete = response.Complete && binsComplete

	template := templateBin(request.Worker)
	reasons := map[string]int64{}
	compatible := make([]item, 0, len(items))
	for _, candidate := range items {
		if candidate.reason != "" {
			reasons[candidate.reason]++
			continue
		}
		if reason := incompatibility(candidate, template); reason != "" {
			reasons[reason]++
			continue
		}
		compatible = append(compatible, candidate)
	}
	response.CompatibleQueuedItems = int64(len(compatible))
	for _, count := range reasons {
		response.IncompatibleQueuedItems += count
	}
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Strings(keys)
	for _, reason := range keys {
		response.Incompatibilities = append(response.Incompatibilities, capacityapi.CapacityIncompatibility{Reason: reason, Count: reasons[reason]})
		if reasonBlocksScaleIn(reason) {
			response.ScaleInBlocked = true
		}
	}
	if !binsComplete {
		return response, nil
	}

	sort.SliceStable(compatible, func(i, j int) bool {
		left, right := compatible[i], compatible[j]
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

	created := int32(0)
	for _, candidate := range compatible {
		placed := false
		for index := range bins {
			if place(&bins[index], candidate) {
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		if created >= request.MaxAdditionalWorkers {
			response.Saturated = true
			continue
		}
		fresh := template
		if !place(&fresh, candidate) {
			return capacityapi.CapacityPlanResponse{}, errors.New("compatible candidate did not fit the Worker template")
		}
		bins = append(bins, fresh)
		created++
	}
	response.RecommendedAdditionalWorkers = created
	return response, nil
}

func reasonBlocksScaleIn(reason string) bool {
	switch reason {
	case reasonRetainedRuntime, reasonQueueConcurrency, reasonRuntimeCompatibility:
		return true
	default:
		return false
	}
}

func discoverItems(ctx context.Context, store Store, group db.WorkerGroup, scanSeed string) ([]item, bool, error) {
	result := make([]item, 0)
	complete := true
	if group.AllowsRun {
		remaining := maximumPlanningCandidates
		var after db.ListQueuedRunCandidateScopesRow
		var visited int32
		for remaining > 0 && visited < maximumPlanningScopes {
			pageLimit := min(planningScopePageSize, maximumPlanningScopes-visited)
			scopes, err := store.ListQueuedRunCandidateScopes(ctx, db.ListQueuedRunCandidateScopesParams{
				AfterSortKey: after.SortKey, AfterOrgID: after.OrgID, AfterProjectID: after.ProjectID,
				AfterEnvironmentID: after.EnvironmentID, AfterRegionID: after.RegionID,
				AfterConcurrencyKey: after.ConcurrencyKey, AfterQueueName: after.QueueName,
				RowLimit: pageLimit, ScanSeed: scanSeed, RegionFilter: group.RegionID,
			})
			if err != nil {
				return nil, false, fmt.Errorf("list capacity planning run scopes: %w", err)
			}
			if len(scopes) == 0 {
				break
			}
			visited += int32(len(scopes))
			for _, scope := range scopes {
				if remaining <= 0 {
					complete = false
					break
				}
				rows, err := store.ListQueuedRunDispatchCandidatesForScope(ctx, db.ListQueuedRunDispatchCandidatesForScopeParams{
					OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
					RegionID: scope.RegionID, ConcurrencyKey: optionalText(scope.ConcurrencyKey), QueueName: scope.QueueName,
					RowLimit: remaining + 1,
				})
				if err != nil {
					return nil, false, fmt.Errorf("list capacity planning run candidates: %w", err)
				}
				if len(rows) > int(remaining) {
					complete = false
					rows = rows[:remaining]
				}
				admission := queueAdmissionStateFromScope(scope)
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
	if group.AllowsBuild {
		remaining := maximumPlanningCandidates
		rows, err := store.ListQueuedDeploymentBuildCandidates(ctx, db.ListQueuedDeploymentBuildCandidatesParams{
			BuildRegionID: group.RegionID, LimitCount: remaining + 1,
		})
		if err != nil {
			return nil, false, fmt.Errorf("list capacity planning build candidates: %w", err)
		}
		if len(rows) > int(remaining) {
			complete = false
			rows = rows[:remaining]
		}
		resources := compute.BuildEnvelopeResources()
		for _, row := range rows {
			result = append(result, item{role: "build", key: fmt.Sprintf("%x", row.DeploymentID.Bytes), resources: capacityapi.ResourceVector{
				CPUMillis: resources.MilliCPU, MemoryBytes: resources.MemoryMiB * mebibyte,
				GuestEphemeralDiskBytes: resources.DiskMiB * mebibyte, BuildExecutors: 1,
			}})
		}
	}
	return result, complete, nil
}

func validateTemplateForGroup(manifest capacityapi.WorkerReleaseManifest, group db.WorkerGroup) error {
	if manifest.SupportsRun && !group.AllowsRun {
		return errors.New("run role is not allowed")
	}
	if manifest.SupportsBuild && !group.AllowsBuild {
		return errors.New("build role is not allowed")
	}
	if manifest.Capacity.CPUMillis < group.RequiredCPUMillis ||
		manifest.Capacity.MemoryBytes < group.RequiredMemoryBytes ||
		manifest.Capacity.GuestEphemeralDiskBytes < group.RequiredGuestEphemeralDiskBytes ||
		manifest.BuildCacheBytes < group.RequiredBuildCacheBytes ||
		manifest.ArtifactCacheBytes < group.RequiredArtifactCacheBytes {
		return errors.New("shared capacity is below the Worker Group requirement")
	}
	if manifest.SupportsRun && manifest.Capacity.VMSlots < int64(group.RequiredVMSlots) {
		return errors.New("VM slots are below the Worker Group requirement")
	}
	return nil
}

type queueAdmissionState struct {
	activeRuns, activeLimit     int64
	preparedRuns, preparedLimit int64
	admitted                    int64
}

func queueAdmissionStateFromScope(scope db.ListQueuedRunCandidateScopesRow) queueAdmissionState {
	return queueAdmissionState{
		activeRuns: scope.ActiveRuns, activeLimit: scope.ActiveLimit,
		preparedRuns: scope.PreparedRuns, preparedLimit: scope.PreparedLimit,
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

func runItem(row db.ListQueuedRunDispatchCandidatesForScopeRow) item {
	result := item{role: "run", key: fmt.Sprintf("%x", row.RunID.Bytes)}
	if row.RequiresRetainedRuntime {
		result.reason = reasonRetainedRuntime
		return result
	}
	var manifest deployment.SandboxManifest
	decoder := json.NewDecoder(bytes.NewReader(row.WorkspaceManifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		result.reason = reasonInvalidWorkload
		return result
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		result.reason = reasonInvalidWorkload
		return result
	}
	if manifest.Resources.MilliCPU <= 0 || manifest.Resources.MemoryMiB <= 0 ||
		manifest.Resources.MemoryMiB > math.MaxInt64/mebibyte {
		result.reason = reasonInvalidWorkload
		return result
	}
	result.resources = capacityapi.ResourceVector{
		CPUMillis: manifest.Resources.MilliCPU, MemoryBytes: manifest.Resources.MemoryMiB * mebibyte,
		GuestEphemeralDiskBytes: compute.WorkspaceGuestEphemeralDiskMiB * mebibyte,
		VMSlots:                 1,
	}
	result.runtimeIdentityID = row.RequiredRuntimeIdentityID
	result.substrateFormat = row.RequiredSubstrateFormat
	result.substrateContract = row.RequiredSubstrateContract
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
	return bin{
		resources: capacityapi.ResourceVector{
			CPUMillis: row.AvailableCPUMillis, MemoryBytes: row.AvailableMemoryBytes,
			GuestEphemeralDiskBytes: row.AvailableGuestEphemeralDiskBytes,
			VMSlots:                 row.AvailableVMSlots,
			BuildExecutors:          row.AvailableBuildExecutors,
		},
		runConsumers: row.AvailableRunConsumers, runtimeStarts: row.AvailableRuntimeStarts, supportsRun: row.SupportsRun,
		supportsBuild: row.SupportsBuild, runtimeArch: row.RuntimeArch,
		runtimeContract: row.VMRuntimeContract, runtimeIdentityID: row.RuntimeIdentityID.String,
		substrateFormat: row.SubstrateFormat, substrateContract: row.SubstrateContract,
		runPaused: row.RunPausedReason.Valid, buildPaused: row.BuildPausedReason.Valid,
		runtimePaused: row.RuntimePausedReason.Valid,
		perVM: capacityapi.ResourceVector{
			CPUMillis: row.PerVMCPUMillis, MemoryBytes: row.PerVMMemoryBytes,
			GuestEphemeralDiskBytes: row.PerVMGuestEphemeralDiskBytes,
		},
	}
}

func templateBin(manifest capacityapi.WorkerReleaseManifest) bin {
	return bin{
		resources: manifest.Capacity, runConsumers: manifest.Capacity.VMSlots, runtimeStarts: manifest.Capacity.VMSlots,
		supportsRun: manifest.SupportsRun, supportsBuild: manifest.SupportsBuild,
		runtimeArch: manifest.Runtime.Arch, runtimeContract: manifest.Runtime.Contract,
		runtimeIdentityID: manifest.Runtime.ID, substrateFormat: manifest.Substrate.Format,
		substrateContract: manifest.Substrate.Contract,
		perVM:             manifest.PerVM,
	}
}

type RunRequirements struct {
	Resources         capacityapi.ResourceVector
	Architecture      string
	RuntimeIdentityID string
	SubstrateFormat   string
	SubstrateContract string
}

func SelectRunWorker(rows []db.ListWorkerCapacityBinsRow, request RunRequirements) (db.ListWorkerCapacityBinsRow, bool) {
	for _, row := range rows {
		target := binFromRow(row)
		candidate := item{role: "run", resources: request.Resources}
		if target.runPaused || target.runtimePaused || incompatibility(candidate, target) != "" {
			continue
		}
		if request.Architecture != "" && target.runtimeArch != request.Architecture {
			continue
		}
		if request.RuntimeIdentityID != "" && target.runtimeIdentityID != request.RuntimeIdentityID {
			continue
		}
		if request.SubstrateFormat != "" && target.substrateFormat != request.SubstrateFormat {
			continue
		}
		if request.SubstrateContract != "" && target.substrateContract != request.SubstrateContract {
			continue
		}
		if target.runtimeStarts <= 0 {
			continue
		}
		return row, true
	}
	return db.ListWorkerCapacityBinsRow{}, false
}

func SelectBuildWorker(rows []db.ListWorkerCapacityBinsRow) (db.ListWorkerCapacityBinsRow, bool) {
	envelope := compute.BuildEnvelopeResources()
	guest := compute.BuildGuestResources()
	request := item{role: "build", resources: capacityapi.ResourceVector{
		CPUMillis: envelope.MilliCPU, MemoryBytes: envelope.MemoryMiB * mebibyte,
		GuestEphemeralDiskBytes: envelope.DiskMiB * mebibyte, BuildExecutors: 1,
	}}
	for _, row := range rows {
		target := binFromRow(row)
		if target.buildPaused || incompatibility(request, target) != "" {
			continue
		}
		guestRequest := capacityapi.ResourceVector{
			CPUMillis: guest.MilliCPU, MemoryBytes: guest.MemoryMiB * mebibyte,
			GuestEphemeralDiskBytes: guest.DiskMiB * mebibyte,
		}
		if !fitsPhysical(target.perVM, guestRequest) {
			continue
		}
		return row, true
	}
	return db.ListWorkerCapacityBinsRow{}, false
}

func incompatibility(candidate item, target bin) string {
	if candidate.role == "run" && !target.supportsRun {
		return reasonRunRole
	}
	if candidate.role == "build" && !target.supportsBuild {
		return reasonBuildRole
	}
	if target.runtimeArch != "x86_64" || target.runtimeContract != "helmr.vm-runtime.v0" {
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
	if candidate.role == "build" {
		guest := compute.BuildGuestResources()
		if !fitsPhysical(target.perVM, capacityapi.ResourceVector{
			CPUMillis: guest.MilliCPU, MemoryBytes: guest.MemoryMiB * mebibyte,
			GuestEphemeralDiskBytes: guest.DiskMiB * mebibyte,
		}) {
			return reasonPerInstanceResources
		}
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
	if candidate.role == "build" && target.buildPaused {
		return false
	}
	target.resources.CPUMillis -= candidate.resources.CPUMillis
	target.resources.MemoryBytes -= candidate.resources.MemoryBytes
	target.resources.GuestEphemeralDiskBytes -= candidate.resources.GuestEphemeralDiskBytes
	if candidate.role == "run" {
		target.resources.VMSlots--
		target.runConsumers--
		target.runtimeStarts--
	} else {
		target.resources.BuildExecutors--
	}
	return true
}

func fitsResources(available, required capacityapi.ResourceVector) bool {
	return available.CPUMillis >= required.CPUMillis &&
		available.MemoryBytes >= required.MemoryBytes &&
		available.GuestEphemeralDiskBytes >= required.GuestEphemeralDiskBytes &&
		available.VMSlots >= required.VMSlots &&
		available.BuildExecutors >= required.BuildExecutors
}

func fitsPhysical(available, required capacityapi.ResourceVector) bool {
	return available.CPUMillis >= required.CPUMillis &&
		available.MemoryBytes >= required.MemoryBytes &&
		available.GuestEphemeralDiskBytes >= required.GuestEphemeralDiskBytes
}

func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}
