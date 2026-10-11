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
)

// statusMutationLockKey serializes one worker group's status mutations and
// its operator host-loss confirmations. The key input is stable and
// provider-neutral.
func statusMutationLockKey(groupID uuid.UUID) int64 {
	return pglock.Key("helmr:worker-group-lifecycle:" + groupID.String())
}

// GroupStatus is a worker group's lifecycle state and claim fence.
// TransitionApplied reports whether a transition changed it rather than
// replayed one that already had.
type GroupStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ClaimVersion      int64  `json:"claim_version"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

// HostStatus is a worker host's lifecycle state and claim fence, located by
// its worker group and provider resource ID.
type HostStatus struct {
	ID                string `json:"id"`
	ResourceID        string `json:"resource_id"`
	WorkerGroupID     string `json:"worker_group_id"`
	Status            string `json:"status"`
	ClaimVersion      int64  `json:"claim_version"`
	CurrentEpoch      *int64 `json:"current_epoch"`
	TransitionApplied bool   `json:"transition_applied,omitempty"`
}

// ReadGroupStatus returns a worker group's lifecycle state.
func ReadGroupStatus(ctx context.Context, q db.Querier, groupID uuid.UUID) (GroupStatus, error) {
	row, err := q.GetWorkerGroupStatus(ctx, pgvalue.UUID(groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupStatus{}, ErrGroupNotFound
	}
	if err != nil {
		return GroupStatus{}, fmt.Errorf("read worker group lifecycle: %w", err)
	}
	return GroupStatus{ID: pgvalue.UUIDString(row.ID), Status: row.Status, ClaimVersion: row.ClaimVersion}, nil
}

// PauseGroup stops new admission onto the worker group's hosts.
func PauseGroup(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroup(ctx, txb, groupID, expectedClaimVersion, db.WorkerGroupStatusPaused)
}

// ActivateGroup resumes admission onto the worker group's hosts.
func ActivateGroup(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroup(ctx, txb, groupID, expectedClaimVersion, db.WorkerGroupStatusActive)
}

// BeginGroupDrain starts draining the worker group and clears its primary
// pool after all physical custody and retained checkpoints have been resolved.
func BeginGroupDrain(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroup(ctx, txb, groupID, expectedClaimVersion, db.WorkerGroupStatusDraining)
}

// DisableGroup disables the worker group.
func DisableGroup(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, expectedClaimVersion int64) (GroupStatus, error) {
	return transitionGroup(ctx, txb, groupID, expectedClaimVersion, db.WorkerGroupStatusDisabled)
}

// transitionGroup moves a worker group to target under the group's status
// mutation lock. Replaying a transition with the claim version that fenced it
// succeeds without applying it again.
func transitionGroup(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, expectedClaimVersion int64, target db.WorkerGroupStatus) (GroupStatus, error) {
	if expectedClaimVersion <= 0 {
		return GroupStatus{}, invalidInput("expected_claim_version must be positive")
	}
	var status GroupStatus
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockWorkerGroupMutation(ctx, statusMutationLockKey(groupID)); err != nil {
			return fmt.Errorf("lock worker group lifecycle: %w", err)
		}
		if _, err := ReadGroupStatus(ctx, q, groupID); err != nil {
			return err
		}
		if target == db.WorkerGroupStatusDraining {
			// Take the same row lock as allocation and capture before reading
			// dependencies in a new statement, including changes committed while
			// waiting for this lock under READ COMMITTED.
			if _, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(groupID)); err != nil {
				return fmt.Errorf("lock worker group drain: %w", err)
			}
			var retained bool
			if err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=$1 AND l.fenced_at IS NULL)
 OR EXISTS(SELECT 1 FROM computer_preparations p JOIN worker_hosts h ON h.id=p.worker_host_id
 WHERE h.worker_group_id=$1 AND p.fenced_at IS NULL)
 OR EXISTS(SELECT 1 FROM computer_checkpoints c
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,c.source_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=$1 AND c.status IN ('capturing','sealed','ready','restoring','aborting'))`, groupID).Scan(&retained); err != nil {
				return fmt.Errorf("read worker group dependencies: %w", err)
			}
			if retained {
				return conflict("worker group retains physical custody or Computer checkpoints")
			}
		}
		row, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
			WorkerGroupID: pgvalue.UUID(groupID), ExpectedClaimVersion: expectedClaimVersion, TargetStatus: target,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker group state or claim version changed")
		}
		if err != nil {
			return fmt.Errorf("transition worker group lifecycle: %w", err)
		}
		status = GroupStatus{
			ID: pgvalue.UUIDString(row.ID), Status: row.Status, ClaimVersion: row.ClaimVersion,
			TransitionApplied: row.TransitionApplied,
		}
		return nil
	})
	if err != nil {
		return GroupStatus{}, err
	}
	return status, nil
}

// ReadHostStatus returns the lifecycle state of the worker group's host with
// the provider resource ID.
func ReadHostStatus(ctx context.Context, q db.Querier, groupID uuid.UUID, resourceID string) (HostStatus, error) {
	resourceID, err := hostResourceID(resourceID)
	if err != nil {
		return HostStatus{}, err
	}
	row, err := q.GetWorkerHostStatusByResource(ctx, db.GetWorkerHostStatusByResourceParams{
		WorkerGroupID: pgvalue.UUID(groupID), ResourceID: resourceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostStatus{}, ErrHostNotFound
	}
	if err != nil {
		return HostStatus{}, fmt.Errorf("read worker host lifecycle: %w", err)
	}
	status := HostStatus{
		ID: pgvalue.UUIDString(row.ID), ResourceID: row.ResourceID, WorkerGroupID: pgvalue.UUIDString(row.WorkerGroupID),
		Status: row.Status, ClaimVersion: row.ClaimVersion,
	}
	if row.CurrentEpoch.Valid {
		status.CurrentEpoch = &row.CurrentEpoch.Int64
	}
	return status, nil
}

// MarkHostLost records an operator's confirmation that the worker group's host
// with the provider resource ID is gone, under the group's status mutation
// lock.
func MarkHostLost(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, resourceID string, expectedClaimVersion int64) (HostStatus, error) {
	resourceID, err := hostResourceID(resourceID)
	if err != nil {
		return HostStatus{}, err
	}
	if expectedClaimVersion <= 0 {
		return HostStatus{}, invalidInput("expected_claim_version must be positive")
	}
	var status HostStatus
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockWorkerGroupMutation(ctx, statusMutationLockKey(groupID)); err != nil {
			return fmt.Errorf("lock worker group lifecycle: %w", err)
		}
		row, err := q.MarkWorkerHostLost(ctx, db.MarkWorkerHostLostParams{
			WorkerGroupID: pgvalue.UUID(groupID), ResourceID: resourceID, ExpectedClaimVersion: expectedClaimVersion,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict(fmt.Sprintf("worker host %q/%q state or claim version changed", groupID.String(), resourceID))
		}
		if err != nil {
			return fmt.Errorf("mark worker host lost: %w", err)
		}
		status = HostStatus{
			ID: pgvalue.UUIDString(row.ID), ResourceID: row.ResourceID, WorkerGroupID: pgvalue.UUIDString(row.WorkerGroupID),
			Status: row.Status, ClaimVersion: row.ClaimVersion, TransitionApplied: row.TransitionApplied,
		}
		if row.CurrentEpoch.Valid {
			status.CurrentEpoch = &row.CurrentEpoch.Int64
		}
		return nil
	})
	if err != nil {
		return HostStatus{}, err
	}
	return status, nil
}

func hostResourceID(resourceID string) (string, error) {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" || len(resourceID) > MaxResourceIDBytes {
		return "", invalidInput("worker resource id is required and must not exceed %d bytes", MaxResourceIDBytes)
	}
	return resourceID, nil
}
