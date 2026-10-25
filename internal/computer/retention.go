package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/lifetime"
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
	rows, err := database.Query(ctx, `SELECT environment_id,id FROM computers c WHERE deleted_at IS NULL AND delete_after_unused_ms IS NOT NULL AND (unused_since IS NULL OR extract(epoch FROM (clock_timestamp()-unused_since))*1000>=delete_after_unused_ms OR EXISTS(SELECT 1 FROM computer_saves save WHERE save.environment_id=c.environment_id AND save.computer_id=c.id AND save.requested_at>=c.unused_since)) AND (environment_id,id)>($1,$2) ORDER BY environment_id,id LIMIT 100`, after.environment, after.computer)
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

func reconcileUnused(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxBeginner, env, id uuid.UUID) (bool, error) {
	deletedByRetention := false
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var deleted bool
		var policy *int64
		if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,delete_after_unused_ms FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&deleted, &policy); err != nil {
			return err
		}
		if deleted || policy == nil {
			return nil
		}
		var busy bool
		if err := tx.QueryRow(ctx, computerBusySQL, env, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			_, err := tx.Exec(ctx, `UPDATE computers SET unused_since=NULL WHERE environment_id=$1 AND id=$2`, env, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE computers SET unused_since=CASE WHEN unused_since IS NULL OR EXISTS(SELECT 1 FROM computer_saves save WHERE save.environment_id=computers.environment_id AND save.computer_id=computers.id AND save.requested_at>=computers.unused_since) THEN clock_timestamp() ELSE unused_since END WHERE environment_id=$1 AND id=$2`, env, id); err != nil {
			return err
		}
		var due bool
		if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM (clock_timestamp()-unused_since))*1000>=delete_after_unused_ms FROM computers WHERE environment_id=$1 AND id=$2`, env, id).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		if err := deleteLocked(ctx, tx, env, id, false); err != nil {
			return err
		}
		if err := audit.Append(ctx, tx, auditPolicy, audit.Event{Actor: audit.System("computer-retention"), EnvironmentID: env, ComputerID: id, Action: "computer.deleted", TargetKind: "computer", TargetID: id}); err != nil {
			return err
		}
		deletedByRetention = true
		return nil
	})
	return deletedByRetention && err == nil, err
}

// UpdateRetention serializes preservation with deletion. The current continuous
// unused interval remains valid when a policy changes; deleted data cannot be revived.
func UpdateRetention(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxBeginner, principal auth.Principal, scope Scope, id uuid.UUID, policy lifetime.Policy) (Snapshot, error) {
	if !policy.Set || !policy.Valid() {
		return Snapshot{}, invalidInput("delete_after_unused_ms must be null or positive safe integer milliseconds")
	}
	var result Snapshot
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var deleted bool
		var before *int64
		if err := tx.QueryRow(ctx, `SELECT c.deleted_at IS NOT NULL,c.delete_after_unused_ms FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.environment_id=$1 AND c.id=$2 AND e.org_id=$3 AND e.project_id=$4 FOR NO KEY UPDATE OF c`, scope.EnvironmentID, id, scope.OrgID, scope.ProjectID).Scan(&deleted, &before); err != nil {
			return err
		}
		if deleted {
			return ErrNotFound
		}
		actor, err := operationActor(ctx, tx, principal, scope, auth.PermissionComputersUpdateRetention)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE computers SET delete_after_unused_ms=$3,updated_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, scope.EnvironmentID, id, policy.Milliseconds); err != nil {
			return err
		}
		if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Actor: actor, EnvironmentID: scope.EnvironmentID, ComputerID: id, Action: "computer.retention_changed", TargetKind: "computer", TargetID: id, Details: audit.ComputerRetentionDetails{Before: before, After: policy.Milliseconds}}); err != nil {
			return err
		}
		result, err = Read(ctx, tx, scope, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return result, err
}
