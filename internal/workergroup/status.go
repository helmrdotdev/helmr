package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrStatusConflict = errors.New("worker lifecycle fence conflict")

// StatusMutationLockKey serializes one logical Worker group's status mutations
// with deployment provider mutations. The lock-key input is intentionally
// stable and provider-neutral.
func StatusMutationLockKey(groupID uuid.UUID) int64 {
	return pglock.Key("helmr:worker-group-lifecycle:" + groupID.String())
}

type StatusStore interface {
	GetWorkerGroupStatus(context.Context, pgtype.UUID) (db.GetWorkerGroupStatusRow, error)
	TransitionWorkerGroupStatus(context.Context, db.TransitionWorkerGroupStatusParams) (db.TransitionWorkerGroupStatusRow, error)
	GetWorkerInstanceStatusByResource(context.Context, db.GetWorkerInstanceStatusByResourceParams) (db.GetWorkerInstanceStatusByResourceRow, error)
	MarkWorkerInstanceLost(context.Context, db.MarkWorkerInstanceLostParams) (db.MarkWorkerInstanceLostRow, error)
}

type GroupStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ClaimVersion      int64  `json:"claim_version"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

type InstanceStatus struct {
	ID                string `json:"id"`
	ResourceID        string `json:"resource_id"`
	WorkerGroupID     string `json:"worker_group_id"`
	Status            string `json:"status"`
	ClaimVersion      int64  `json:"claim_version"`
	CurrentEpoch      *int64 `json:"current_epoch"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

func ReadGroupStatus(ctx context.Context, store StatusStore, groupID uuid.UUID) (GroupStatus, error) {
	row, err := store.GetWorkerGroupStatus(ctx, pgvalue.UUID(groupID))
	if err != nil {
		return GroupStatus{}, err
	}
	return GroupStatus{ID: pgvalue.UUIDString(row.ID), Status: row.Status, ClaimVersion: row.ClaimVersion}, nil
}

func PauseGroup(ctx context.Context, store StatusStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupStatus(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStatusPaused)
}

func ActivateGroup(ctx context.Context, store StatusStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupStatus(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStatusActive)
}

func BeginGroupDrain(ctx context.Context, store StatusStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupStatus(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStatusDraining)
}

func DisableGroup(ctx context.Context, store StatusStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupStatus(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStatusDisabled)
}

func transitionGroupStatus(ctx context.Context, store StatusStore, groupID uuid.UUID, expectedClaimVersion int64, targetStatus db.WorkerGroupStatus) (GroupStatus, error) {
	if expectedClaimVersion <= 0 {
		return GroupStatus{}, errors.New("expected claim version must be positive")
	}
	row, err := store.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: pgvalue.UUID(groupID), ExpectedClaimVersion: expectedClaimVersion, TargetStatus: targetStatus,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupStatus{}, fmt.Errorf("%w: worker group %q did not match status/version", ErrStatusConflict, groupID.String())
	}
	if err != nil {
		return GroupStatus{}, err
	}
	return GroupStatus{
		ID: pgvalue.UUIDString(row.ID), Status: row.Status, ClaimVersion: row.ClaimVersion,
		TransitionApplied: row.TransitionApplied,
	}, nil
}

func ReadInstanceStatus(ctx context.Context, store StatusStore, groupID uuid.UUID, resourceID string) (InstanceStatus, error) {
	resourceID, err := statusInstanceLocator(resourceID)
	if err != nil {
		return InstanceStatus{}, err
	}
	row, err := store.GetWorkerInstanceStatusByResource(ctx, db.GetWorkerInstanceStatusByResourceParams{
		WorkerGroupID: pgvalue.UUID(groupID), ResourceID: resourceID,
	})
	if err != nil {
		return InstanceStatus{}, err
	}
	return instanceStatus(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.Status, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, false), nil
}

func MarkInstanceLost(ctx context.Context, store StatusStore, groupID uuid.UUID, resourceID string, expectedClaimVersion int64) (InstanceStatus, error) {
	resourceID, err := statusInstanceLocator(resourceID)
	if err != nil {
		return InstanceStatus{}, err
	}
	if expectedClaimVersion <= 0 {
		return InstanceStatus{}, errors.New("expected claim version must be positive")
	}
	row, err := store.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: pgvalue.UUID(groupID), ResourceID: resourceID, ExpectedClaimVersion: expectedClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceStatus{}, fmt.Errorf("%w: worker instance %q/%q did not match status/version", ErrStatusConflict, groupID.String(), resourceID)
	}
	if err != nil {
		return InstanceStatus{}, err
	}
	return instanceStatus(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.Status, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, row.TransitionApplied), nil
}

func statusInstanceLocator(resourceID string) (string, error) {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" || len(resourceID) > 512 {
		return "", errors.New("worker resource id is required and must not exceed 512 bytes")
	}
	return resourceID, nil
}

func instanceStatus(id [16]byte, resourceID string, groupID pgtype.UUID, status string, claimVersion int64, currentEpoch int64, hasCurrentEpoch bool, transitionApplied bool) InstanceStatus {
	result := InstanceStatus{
		ID: uuid.UUID(id).String(), ResourceID: resourceID, WorkerGroupID: pgvalue.UUIDString(groupID),
		Status: status, ClaimVersion: claimVersion, TransitionApplied: transitionApplied,
	}
	if hasCurrentEpoch {
		result.CurrentEpoch = &currentEpoch
	}
	return result
}
