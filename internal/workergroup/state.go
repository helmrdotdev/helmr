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

var ErrStateConflict = errors.New("worker lifecycle fence conflict")

// StateMutationLockKey serializes one logical Worker group's state mutations
// with deployment provider mutations. The lock-key input is intentionally
// stable and provider-neutral.
func StateMutationLockKey(groupID uuid.UUID) int64 {
	return pglock.Key("helmr:worker-group-lifecycle:" + groupID.String())
}

type StateStore interface {
	GetWorkerGroupState(context.Context, pgtype.UUID) (db.GetWorkerGroupStateRow, error)
	TransitionWorkerGroupState(context.Context, db.TransitionWorkerGroupStateParams) (db.TransitionWorkerGroupStateRow, error)
	GetWorkerInstanceStateByResource(context.Context, db.GetWorkerInstanceStateByResourceParams) (db.GetWorkerInstanceStateByResourceRow, error)
	MarkWorkerInstanceLost(context.Context, db.MarkWorkerInstanceLostParams) (db.MarkWorkerInstanceLostRow, error)
}

type GroupStatus struct {
	ID                string `json:"id"`
	State             string `json:"state"`
	ClaimVersion      int64  `json:"claim_version"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

type InstanceStatus struct {
	ID                string `json:"id"`
	ResourceID        string `json:"resource_id"`
	WorkerGroupID     string `json:"worker_group_id"`
	State             string `json:"state"`
	ClaimVersion      int64  `json:"claim_version"`
	CurrentEpoch      *int64 `json:"current_epoch"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

func ReadGroupStatus(ctx context.Context, store StateStore, groupID uuid.UUID) (GroupStatus, error) {
	row, err := store.GetWorkerGroupState(ctx, pgvalue.UUID(groupID))
	if err != nil {
		return GroupStatus{}, err
	}
	return GroupStatus{ID: pgvalue.UUIDString(row.ID), State: row.State, ClaimVersion: row.ClaimVersion}, nil
}

func PauseGroup(ctx context.Context, store StateStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupState(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStatePaused)
}

func ActivateGroup(ctx context.Context, store StateStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupState(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStateActive)
}

func BeginGroupDrain(ctx context.Context, store StateStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupState(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStateDraining)
}

func DisableGroup(ctx context.Context, store StateStore, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroupState(ctx, store, groupID, expectedClaimVersion, db.WorkerGroupStateDisabled)
}

func transitionGroupState(ctx context.Context, store StateStore, groupID uuid.UUID, expectedClaimVersion int64, targetState db.WorkerGroupState) (GroupStatus, error) {
	if expectedClaimVersion <= 0 {
		return GroupStatus{}, errors.New("expected claim version must be positive")
	}
	row, err := store.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: pgvalue.UUID(groupID), ExpectedClaimVersion: expectedClaimVersion, TargetState: targetState,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupStatus{}, fmt.Errorf("%w: worker group %q did not match state/version", ErrStateConflict, groupID.String())
	}
	if err != nil {
		return GroupStatus{}, err
	}
	return GroupStatus{
		ID: pgvalue.UUIDString(row.ID), State: row.State, ClaimVersion: row.ClaimVersion,
		TransitionApplied: row.TransitionApplied,
	}, nil
}

func ReadInstanceStatus(ctx context.Context, store StateStore, groupID uuid.UUID, resourceID string) (InstanceStatus, error) {
	resourceID, err := stateInstanceLocator(resourceID)
	if err != nil {
		return InstanceStatus{}, err
	}
	row, err := store.GetWorkerInstanceStateByResource(ctx, db.GetWorkerInstanceStateByResourceParams{
		WorkerGroupID: pgvalue.UUID(groupID), ResourceID: resourceID,
	})
	if err != nil {
		return InstanceStatus{}, err
	}
	return instanceStatus(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, false), nil
}

func MarkInstanceLost(ctx context.Context, store StateStore, groupID uuid.UUID, resourceID string, expectedClaimVersion int64) (InstanceStatus, error) {
	resourceID, err := stateInstanceLocator(resourceID)
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
		return InstanceStatus{}, fmt.Errorf("%w: worker instance %q/%q did not match state/version", ErrStateConflict, groupID.String(), resourceID)
	}
	if err != nil {
		return InstanceStatus{}, err
	}
	return instanceStatus(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, row.TransitionApplied), nil
}

func stateInstanceLocator(resourceID string) (string, error) {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" || len(resourceID) > 512 {
		return "", errors.New("worker resource id is required and must not exceed 512 bytes")
	}
	return resourceID, nil
}

func instanceStatus(id [16]byte, resourceID string, groupID pgtype.UUID, state string, claimVersion int64, currentEpoch int64, hasCurrentEpoch bool, transitionApplied bool) InstanceStatus {
	result := InstanceStatus{
		ID: uuid.UUID(id).String(), ResourceID: resourceID, WorkerGroupID: pgvalue.UUIDString(groupID),
		State: state, ClaimVersion: claimVersion, TransitionApplied: transitionApplied,
	}
	if hasCurrentEpoch {
		result.CurrentEpoch = &currentEpoch
	}
	return result
}
