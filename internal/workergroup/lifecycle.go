package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sessionlock"
	"github.com/jackc/pgx/v5"
)

var ErrLifecycleConflict = errors.New("worker lifecycle fence conflict")

// LifecycleLockKey serializes one logical Worker group's deployment lifecycle
// transitions with deployment provider mutations. It is intentionally provider-neutral.
func LifecycleLockKey(groupID string) int64 {
	return sessionlock.Key("helmr:worker-group-lifecycle:" + strings.TrimSpace(groupID))
}

type LifecycleStore interface {
	GetWorkerGroupLifecycle(context.Context, string) (db.GetWorkerGroupLifecycleRow, error)
	TransitionWorkerGroupLifecycle(context.Context, db.TransitionWorkerGroupLifecycleParams) (db.TransitionWorkerGroupLifecycleRow, error)
	GetWorkerInstanceLifecycle(context.Context, db.GetWorkerInstanceLifecycleParams) (db.GetWorkerInstanceLifecycleRow, error)
	MarkWorkerInstanceLost(context.Context, db.MarkWorkerInstanceLostParams) (db.MarkWorkerInstanceLostRow, error)
}

type GroupLifecycle struct {
	ID                string `json:"id"`
	State             string `json:"state"`
	ClaimVersion      int64  `json:"claim_version"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

type InstanceLifecycle struct {
	ID                string `json:"id"`
	ResourceID        string `json:"resource_id"`
	WorkerGroupID     string `json:"worker_group_id"`
	State             string `json:"state"`
	ClaimVersion      int64  `json:"claim_version"`
	CurrentEpoch      *int64 `json:"current_epoch"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

func InspectGroupLifecycle(ctx context.Context, store LifecycleStore, groupID string) (GroupLifecycle, error) {
	groupID, err := lifecycleGroupID(groupID)
	if err != nil {
		return GroupLifecycle{}, err
	}
	row, err := store.GetWorkerGroupLifecycle(ctx, groupID)
	if err != nil {
		return GroupLifecycle{}, err
	}
	return GroupLifecycle{ID: row.ID, State: row.State, ClaimVersion: row.ClaimVersion}, nil
}

func TransitionGroupLifecycle(ctx context.Context, store LifecycleStore, groupID string, expectedClaimVersion int64, targetState string) (GroupLifecycle, error) {
	groupID, err := lifecycleGroupID(groupID)
	if err != nil {
		return GroupLifecycle{}, err
	}
	if expectedClaimVersion <= 0 {
		return GroupLifecycle{}, errors.New("expected claim version must be positive")
	}
	if targetState != string(db.WorkerGroupStateActive) && targetState != string(db.WorkerGroupStatePaused) &&
		targetState != string(db.WorkerGroupStateDraining) && targetState != string(db.WorkerGroupStateDisabled) {
		return GroupLifecycle{}, errors.New("worker group lifecycle target must be active, paused, draining, or disabled")
	}
	row, err := store.TransitionWorkerGroupLifecycle(ctx, db.TransitionWorkerGroupLifecycleParams{
		WorkerGroupID: groupID, ExpectedClaimVersion: expectedClaimVersion, TargetState: targetState,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupLifecycle{}, fmt.Errorf("%w: worker group %q did not match state/version", ErrLifecycleConflict, groupID)
	}
	if err != nil {
		return GroupLifecycle{}, err
	}
	return GroupLifecycle{
		ID: row.ID, State: row.State, ClaimVersion: row.ClaimVersion,
		TransitionApplied: row.TransitionApplied,
	}, nil
}

func InspectInstanceLifecycle(ctx context.Context, store LifecycleStore, groupID string, resourceID string) (InstanceLifecycle, error) {
	groupID, resourceID, err := lifecycleInstanceLocator(groupID, resourceID)
	if err != nil {
		return InstanceLifecycle{}, err
	}
	row, err := store.GetWorkerInstanceLifecycle(ctx, db.GetWorkerInstanceLifecycleParams{
		WorkerGroupID: groupID, ResourceID: resourceID,
	})
	if err != nil {
		return InstanceLifecycle{}, err
	}
	return instanceLifecycle(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, false), nil
}

func MarkInstanceLost(ctx context.Context, store LifecycleStore, groupID string, resourceID string, expectedClaimVersion int64) (InstanceLifecycle, error) {
	groupID, resourceID, err := lifecycleInstanceLocator(groupID, resourceID)
	if err != nil {
		return InstanceLifecycle{}, err
	}
	if expectedClaimVersion <= 0 {
		return InstanceLifecycle{}, errors.New("expected claim version must be positive")
	}
	row, err := store.MarkWorkerInstanceLost(ctx, db.MarkWorkerInstanceLostParams{
		WorkerGroupID: groupID, ResourceID: resourceID, ExpectedClaimVersion: expectedClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceLifecycle{}, fmt.Errorf("%w: worker instance %q/%q did not match state/version", ErrLifecycleConflict, groupID, resourceID)
	}
	if err != nil {
		return InstanceLifecycle{}, err
	}
	return instanceLifecycle(row.ID.Bytes, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion, row.CurrentEpoch.Int64, row.CurrentEpoch.Valid, row.TransitionApplied), nil
}

func lifecycleGroupID(groupID string) (string, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return "", errors.New("worker group id is required")
	}
	return groupID, nil
}

func lifecycleInstanceLocator(groupID string, resourceID string) (string, string, error) {
	groupID, err := lifecycleGroupID(groupID)
	if err != nil {
		return "", "", err
	}
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" || len(resourceID) > 512 {
		return "", "", errors.New("worker resource id is required and must not exceed 512 bytes")
	}
	return groupID, resourceID, nil
}

func instanceLifecycle(id [16]byte, resourceID string, groupID string, state string, claimVersion int64, currentEpoch int64, hasCurrentEpoch bool, transitionApplied bool) InstanceLifecycle {
	result := InstanceLifecycle{
		ID: uuid.UUID(id).String(), ResourceID: resourceID, WorkerGroupID: groupID,
		State: state, ClaimVersion: claimVersion, TransitionApplied: transitionApplied,
	}
	if hasCurrentEpoch {
		result.CurrentEpoch = &currentEpoch
	}
	return result
}
