package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type retentionPosition struct{ environment, computer uuid.UUID }

func RunRetention(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxDB, log *slog.Logger) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var after retentionPosition
	for {
		next, more, err := reconcileRetention(ctx, auditPolicy, database, log, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Computer retention reconciliation failed", "error", err)
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func reconcileRetention(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxDB, log *slog.Logger, after retentionPosition) (retentionPosition, bool, error) {
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id FROM computers c JOIN template_definitions d ON (d.environment_id,d.template_id,d.deployment_id)=(c.environment_id,c.origin_template_id,c.origin_deployment_id)
 WHERE c.deleted_at IS NULL AND d.delete_after_unused_ms IS NOT NULL AND (c.unused_since IS NULL OR extract(epoch FROM (clock_timestamp()-c.unused_since))*1000>=d.delete_after_unused_ms)
 AND (c.environment_id,c.id)>($1,$2) ORDER BY c.environment_id,c.id LIMIT 100`, after.environment, after.computer)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retentionPosition, error) {
		var p retentionPosition
		err := row.Scan(&p.environment, &p.computer)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	var failures []error
	for _, p := range candidates {
		after = p
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		deleted, err := reconcileUnused(attempt, auditPolicy, database, p.environment, p.computer)
		cancel()
		if err != nil {
			failures = append(failures, err)
		} else if deleted {
			log.InfoContext(ctx, "Computer deleted after unused retention", "environment_id", p.environment, "computer_id", p.computer)
		}
	}
	more := len(candidates) == 100
	if !more {
		after = retentionPosition{}
	}
	return after, more, errors.Join(failures...)
}

// reconcileUnused deletes a Computer whose origin Template's unused period has
// passed. The unused clock starts when the Computer has no Session, process or
// Command; a save in progress defers deletion but never restarts the clock.
func reconcileUnused(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxBeginner, env, id uuid.UUID) (bool, error) {
	deletedByRetention := false
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var deleted bool
		var policy *int64
		if err := tx.QueryRow(ctx, `SELECT c.deleted_at IS NOT NULL,d.delete_after_unused_ms FROM computers c JOIN template_definitions d ON (d.environment_id,d.template_id,d.deployment_id)=(c.environment_id,c.origin_template_id,c.origin_deployment_id) WHERE c.environment_id=$1 AND c.id=$2 FOR NO KEY UPDATE OF c`, env, id).Scan(&deleted, &policy); err != nil {
			return err
		}
		if deleted || policy == nil {
			return nil
		}
		var busy, saving bool
		if err := tx.QueryRow(ctx, unusedComputerBusySQL, env, id).Scan(&busy, &saving); err != nil {
			return err
		}
		if busy {
			_, err := tx.Exec(ctx, `UPDATE computers SET unused_since=NULL WHERE environment_id=$1 AND id=$2`, env, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE computers SET unused_since=COALESCE(unused_since,clock_timestamp()) WHERE environment_id=$1 AND id=$2`, env, id); err != nil {
			return err
		}
		if saving {
			return nil
		}
		var due bool
		if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM (clock_timestamp()-unused_since))*1000>=$3 FROM computers WHERE environment_id=$1 AND id=$2`, env, id, *policy).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		if err := deleteLocked(ctx, tx, env, id, false); err != nil {
			return err
		}
		if err := audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), EnvironmentID: env, ComputerID: id, Action: "computer.deleted", TargetKind: "computer", TargetID: id}); err != nil {
			return err
		}
		deletedByRetention = true
		return nil
	})
	return deletedByRetention && err == nil, err
}
