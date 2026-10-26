package audit

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ObservePolicy records one process-start observation, not a policy change.
func ObservePolicy(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, process Component) error {
	if process != ControlPlane && process != Dispatcher {
		return errors.New("unknown audit process")
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		return Append(ctx, tx, policy, Event{Evidence: Internal(process), Action: "audit.retention_observed", TargetKind: "audit", Details: RetentionObservedDetails{RetentionSeconds: policy.RetentionSeconds}})
	})
}

type PurgeResult struct {
	Batches int   `json:"batches"`
	Deleted int64 `json:"deleted"`
}

// Purge deletes bounded expired batches and commits each batch with its receipt.
// No row locks requiring UPDATE authority and no idle success events are used.
func Purge(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, maxBatches int) (PurgeResult, error) {
	result := PurgeResult{}
	if maxBatches < 1 || maxBatches > 1000 {
		return result, errors.New("audit purge batches must be between 1 and 1000")
	}
	if err := policy.Validate(); err != nil {
		return result, err
	}
	for range maxBatches {
		var count int64
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := db.RunTx(batchCtx, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(batchCtx, `SET LOCAL statement_timeout='4s'`); err != nil {
				return err
			}
			var cutoff time.Time
			if err := tx.QueryRow(batchCtx, `SELECT statement_timestamp()`).Scan(&cutoff); err != nil {
				return err
			}
			var minExpiry, maxExpiry *time.Time
			err := tx.QueryRow(batchCtx, `WITH eligible AS (
    SELECT id FROM audit_events WHERE expires_at<=$1 ORDER BY expires_at,id LIMIT 1000
   ), removed AS (
    DELETE FROM audit_events e USING eligible d WHERE e.id=d.id AND e.expires_at<=$1 RETURNING e.expires_at
   ) SELECT count(*),min(expires_at),max(expires_at) FROM removed`, cutoff).Scan(&count, &minExpiry, &maxExpiry)
			if err != nil || count == 0 {
				return err
			}
			return Append(batchCtx, tx, policy, Event{Evidence: Internal(AuditMaintenance), Action: "audit.purge_completed", TargetKind: "audit", Details: PurgeDetails{Cutoff: cutoff, Count: count, MinExpiry: *minExpiry, MaxExpiry: *maxExpiry}})
		})
		cancel()
		if err != nil {
			return result, err
		}
		if count == 0 {
			break
		}
		result.Batches++
		result.Deleted += count
	}
	return result, nil
}

func RecordRestore(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, detail RestoreDetails) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		return Append(ctx, tx, policy, Event{Evidence: Internal(AuditMaintenance), Action: "audit.restore_recorded", TargetKind: "audit", Details: detail})
	})
}
