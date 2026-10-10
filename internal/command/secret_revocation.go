package command

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

// StopSecretRevokedCommands records convergence intent. Revocation quarantines
// the shared Computer separately; this does not claim a process physically stopped.
func StopSecretRevokedCommands(ctx context.Context, database db.TxDB, revocation secret.Revocation, limit int32) (int, error) {
	if err := revocation.Validate(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("secret revocation batch limit must be positive")
	}
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id,c.computer_id,c.revision FROM computer_commands c JOIN environments e ON e.id=c.environment_id
 WHERE c.environment_id=$1 AND c.terminal_at IS NULL AND c.cancel_requested_at IS NULL
 AND EXISTS(SELECT 1 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id)
 WHERE b.environment_id=c.environment_id AND b.computer_id=c.computer_id AND s.id=$2 AND s.status='revoked' AND s.revocation_generation=$3)
 ORDER BY c.created_at,c.id LIMIT $4`, revocation.EnvironmentID, revocation.SecretID, revocation.Generation, limit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecoveryCandidate, error) {
		var c RecoveryCandidate
		err := row.Scan(&c.EnvironmentID, &c.CommandID, &c.ComputerID, &c.ExpectedRevision)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	for i, c := range candidates {
		if err = Recover(ctx, database, c); err != nil && !errors.Is(err, ErrChanged) {
			return i, err
		}
	}
	return len(candidates), nil
}

// RecoverBatch is restart-safe discovery for lost writers and revoked bindings.
// Every candidate is compared again under its immutable Computer/lease locks.
func RecoverBatch(ctx context.Context, database db.TxDB, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("command recovery batch limit must be positive")
	}
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id,c.computer_id,c.revision FROM computer_commands c JOIN environments e ON e.id=c.environment_id
 LEFT JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,c.computer_lease_epoch)
 WHERE (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL AND (l.fenced_at IS NOT NULL OR (c.terminal_at IS NULL AND (l.status IN ('lost','released') OR l.expires_at<=clock_timestamp()))))
 OR (c.terminal_at IS NULL AND c.cancel_requested_at IS NULL AND EXISTS(SELECT 1 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id) WHERE b.environment_id=c.environment_id AND b.computer_id=c.computer_id AND s.status='revoked'))
 ORDER BY c.updated_at,c.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecoveryCandidate, error) {
		var c RecoveryCandidate
		err := row.Scan(&c.EnvironmentID, &c.CommandID, &c.ComputerID, &c.ExpectedRevision)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	for i, c := range candidates {
		if err = Recover(ctx, database, c); err != nil && !errors.Is(err, ErrChanged) {
			return i, err
		}
	}
	return len(candidates), nil
}
