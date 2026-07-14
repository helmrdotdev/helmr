package workergroup

import (
	"context"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type ReconcileStore interface {
	LockWorkerGroupsForReconciliation(context.Context, db.LockWorkerGroupsForReconciliationParams) ([]string, error)
	ReconcileWorkerGroup(context.Context, db.ReconcileWorkerGroupParams) (db.ReconcileWorkerGroupRow, error)
	LockAbsentWorkerGroups(context.Context, db.LockAbsentWorkerGroupsParams) ([]string, error)
	DisableAbsentWorkerGroups(context.Context, db.DisableAbsentWorkerGroupsParams) ([]db.DisableAbsentWorkerGroupsRow, error)
	ListLiveAbsentWorkerGroupIDs(context.Context, db.ListLiveAbsentWorkerGroupIDsParams) ([]string, error)
}

func Reconcile(ctx context.Context, store ReconcileStore, regionID string, desired []Desired) error {
	ids := make([]string, 0, len(desired))
	normalized := make([]Desired, 0, len(desired))
	seen := make(map[string]struct{}, len(desired))
	for _, group := range desired {
		spec, err := Normalize(group.Spec)
		if err != nil {
			return fmt.Errorf("normalize worker group: %w", err)
		}
		if err := group.Capacity.Validate(spec); err != nil {
			return fmt.Errorf("validate worker group %q capacity: %w", spec.ID, err)
		}
		if _, duplicate := seen[spec.ID]; duplicate {
			return fmt.Errorf("worker group %q is duplicated", spec.ID)
		}
		seen[spec.ID] = struct{}{}
		group.Spec = spec
		normalized = append(normalized, group)
		ids = append(ids, spec.ID)
	}
	if _, err := store.LockWorkerGroupsForReconciliation(ctx, db.LockWorkerGroupsForReconciliationParams{
		RegionID: regionID, DesiredIds: ids,
	}); err != nil {
		return fmt.Errorf("lock worker groups for reconciliation: %w", err)
	}
	for _, group := range normalized {
		spec := group.Spec
		if _, err := store.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
			ID: spec.ID, RegionID: regionID, Name: spec.Name, Description: spec.Description,
			AllowsRun: spec.AllowsRun, AllowsBuild: spec.AllowsBuild,
			RequiredCpuMillis: group.Capacity.MilliCPU, RequiredMemoryBytes: group.Capacity.MemoryBytes,
			RequiredWorkloadDiskBytes: group.Capacity.WorkloadDiskBytes, RequiredScratchBytes: group.Capacity.ScratchBytes,
			RequiredBuildCacheBytes: group.Capacity.BuildCacheBytes, RequiredArtifactCacheBytes: group.Capacity.ArtifactCacheBytes,
			RequiredVmSlots: group.Capacity.VMSlots, RequiredBuildExecutors: group.Capacity.BuildExecutors,
			ProtocolVersion:                auth.WorkerProtocolVersion,
			EnrollmentPolicyFingerprint:    group.EnrollmentPolicyFingerprint,
			AllowedAttestationFingerprints: group.AllowedAttestationFingerprints,
			LaunchAttestationFingerprint:   pgvalue.Text(group.LaunchAttestationFingerprint),
		}); err != nil {
			return fmt.Errorf("reconcile worker group %q: %w", spec.ID, err)
		}
	}
	if _, err := store.LockAbsentWorkerGroups(ctx, db.LockAbsentWorkerGroupsParams{
		RegionID: regionID, DesiredIds: ids,
	}); err != nil {
		return fmt.Errorf("lock removed worker groups: %w", err)
	}
	if _, err := store.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{
		RegionID: regionID, DesiredIds: ids,
	}); err != nil {
		return fmt.Errorf("disable removed worker groups: %w", err)
	}
	live, err := store.ListLiveAbsentWorkerGroupIDs(ctx, db.ListLiveAbsentWorkerGroupIDsParams{
		RegionID: regionID, DesiredIds: ids,
	})
	if err != nil {
		return fmt.Errorf("check removed worker groups: %w", err)
	}
	if len(live) > 0 {
		return fmt.Errorf("worker groups %s still have live or fenced instances; drain and terminate every member before removing the group", strings.Join(live, ", "))
	}
	return nil
}
