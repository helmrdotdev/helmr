package workergroup

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrScaleInOperationNotFound = errors.New("scale-in operation not found")

type ScaleInConflict struct {
	Code         string
	OperationKey string
	Blockers     []string
}

func (e ScaleInConflict) Error() string     { return e.Code }
func (e ScaleInConflict) ErrorCode() string { return e.Code }
func (e ScaleInConflict) ErrorDetails() map[string]json.RawMessage {
	result := map[string]json.RawMessage{}
	if e.OperationKey != "" {
		result["operation_key"], _ = json.Marshal(e.OperationKey)
	}
	if e.Blockers != nil {
		result["blockers"], _ = json.Marshal(e.Blockers)
	}
	return result
}

type ScaleInOperation struct {
	OperationKey      string     `json:"operation_key"`
	WorkerGroupID     string     `json:"worker_group_id"`
	WorkerPoolID      string     `json:"worker_pool_id"`
	WorkerHostID      string     `json:"worker_host_id"`
	ResourceID        string     `json:"resource_id"`
	DrainClaimVersion int64      `json:"drain_claim_version"`
	Phase             string     `json:"phase"`
	Blockers          []string   `json:"blockers"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

func GetScaleInOperation(ctx context.Context, q db.DBTX, groupID, key uuid.UUID) (ScaleInOperation, error) {
	var result ScaleInOperation
	var group, pool, host, operation uuid.UUID
	var status string
	var claim int64
	var completed pgtype.Timestamptz
	err := q.QueryRow(ctx, `SELECT o.operation_key,o.worker_group_id,o.worker_pool_id,o.worker_host_id,
 h.resource_id,o.drain_claim_version,o.created_at,o.completed_at,h.status,h.claim_version
 FROM worker_scale_in_operations o JOIN worker_hosts h ON h.id=o.worker_host_id AND h.worker_group_id=o.worker_group_id
 WHERE o.worker_group_id=$1 AND o.operation_key=$2`, groupID, key).Scan(&operation, &group, &pool, &host, &result.ResourceID, &result.DrainClaimVersion, &result.CreatedAt, &completed, &status, &claim)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrScaleInOperationNotFound
	}
	if err != nil {
		return result, err
	}
	result.OperationKey = operation.String()
	result.WorkerGroupID = group.String()
	result.WorkerPoolID = pool.String()
	result.WorkerHostID = host.String()
	result.Blockers = []string{}
	switch {
	case completed.Valid:
		result.Phase = "completed"
		result.CompletedAt = &completed.Time
	case status == "draining" && claim == result.DrainClaimVersion:
		result.Phase = "draining"
	case status == "termination_ready" && claim == result.DrainClaimVersion+1:
		result.Phase = "termination_ready"
	default:
		result.Phase = "blocked"
		result.Blockers = []string{"identity_changed"}
	}
	return result, nil
}

func PendingScaleInOperation(ctx context.Context, q db.DBTX, groupID uuid.UUID) (*ScaleInOperation, error) {
	var key uuid.UUID
	err := q.QueryRow(ctx, `SELECT operation_key FROM worker_scale_in_operations WHERE worker_group_id=$1 AND completed_at IS NULL`, groupID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := GetScaleInOperation(ctx, q, groupID, key)
	return &result, err
}

// SelectScaleIn commits one immutable selection with its drain. A group lock
// serializes keys, ordinary admissions and policy mutations; response loss cannot
// cause the retry to select a second host.
func SelectScaleIn(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, groupID, poolID, key uuid.UUID) (ScaleInOperation, bool, error) {
	if groupID == uuid.Nil() || poolID == uuid.Nil() || key == uuid.Nil() {
		return ScaleInOperation{}, false, invalidInput("group, pool and operation key are required")
	}
	var result ScaleInOperation
	var created bool
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		group, err := lockGroupForPoolMutation(ctx, q, groupID)
		if err != nil {
			return err
		}
		prior, err := GetScaleInOperation(ctx, tx, groupID, key)
		if err == nil {
			if prior.WorkerPoolID != poolID.String() {
				return ScaleInConflict{Code: "scale_in_operation_key_conflict"}
			}
			result = prior
			return nil
		}
		if !errors.Is(err, ErrScaleInOperationNotFound) {
			return err
		}
		pending, err := PendingScaleInOperation(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if pending != nil {
			return ScaleInConflict{Code: "scale_in_operation_pending", OperationKey: pending.OperationKey}
		}
		pool, err := lockPool(ctx, q, groupID, poolID)
		if err != nil {
			return err
		}
		if group.Status != "active" || pool.Status != "active" || !pool.SealedAt.Valid {
			return ScaleInConflict{Code: "scale_in_blocked", Blockers: []string{"no_eligible_host"}}
		}
		queued, err := HasQueuedDemand(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if queued {
			return ScaleInConflict{Code: "scale_in_blocked", Blockers: []string{"queued_work"}}
		}
		// The group lock prevents concurrent allocation admission while selection
		// excludes mandatory custody. Optional seeds are joined during Worker drain.
		var host uuid.UUID
		var epoch, claim int64
		err = tx.QueryRow(ctx, `SELECT h.id,h.current_epoch,h.claim_version FROM worker_hosts h
 WHERE h.worker_group_id=$1 AND h.worker_pool_id=$2 AND h.status='active'
 AND h.current_epoch IS NOT NULL AND h.admission_paused_reason IS NULL
 AND h.observed_at>=clock_timestamp()-$3::bigint*interval '1 second'
 AND NOT EXISTS(SELECT 1 FROM allocation_usage a WHERE a.worker_host_id=h.id AND a.kind NOT IN ('seed','retained'))
 AND NOT EXISTS(SELECT 1 FROM session_processes p JOIN computer_leases l
 ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE l.worker_host_id=h.id AND p.fenced_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=l.environment_id
 AND cp.computer_id=l.computer_id AND cp.source_lease_epoch=l.epoch AND cp.status='ready'))
 ORDER BY h.id LIMIT 1 FOR UPDATE OF h`, groupID, poolID, ObservationFreshnessSeconds).Scan(&host, &epoch, &claim)
		if errors.Is(err, pgx.ErrNoRows) {
			return ScaleInConflict{Code: "scale_in_blocked", Blockers: []string{"no_eligible_host"}}
		}
		if err != nil {
			return err
		}
		// Recheck receive freshness after acquiring the host lock.
		var fresh bool
		if err := tx.QueryRow(ctx, `SELECT observed_at>=clock_timestamp()-$2::bigint*interval '1 second' FROM worker_hosts WHERE id=$1`, host, ObservationFreshnessSeconds).Scan(&fresh); err != nil {
			return err
		}
		if !fresh {
			return ScaleInConflict{Code: "scale_in_blocked", Blockers: []string{"no_eligible_host"}}
		}
		drained, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{ID: pgvalue.UUID(host), WorkerGroupID: pgvalue.UUID(groupID), ExpectedEpoch: pgtype.Int8{Int64: epoch, Valid: true}, ExpectedClaimVersion: claim, DrainReason: string(DrainReasonIdleScaleIn)})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO worker_scale_in_operations(worker_group_id,operation_key,worker_pool_id,worker_host_id,drain_claim_version) VALUES($1,$2,$3,$4,$5)`, groupID, key, poolID, host, drained.ClaimVersion)
		if err != nil {
			return err
		}
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, Action: "worker_host.drain_started", TargetKind: "worker_host", TargetID: host}); err != nil {
			return err
		}
		if err := dbnotify.Notify(ctx, tx, "dispatch", "capacity"); err != nil {
			return err
		}
		result, err = GetScaleInOperation(ctx, tx, groupID, key)
		created = err == nil
		return err
	})
	return result, created, err
}
