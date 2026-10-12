package command

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RecoveryPosition is the last candidate a recovery pass attempted, ordered by
// the Command's last update. The zero value starts a pass.
type RecoveryPosition struct {
	UpdatedAt time.Time
	ID        uuid.UUID
}

// RecoverBatch attempts one page of recovery candidates after the position:
// Commands whose writer was lost and Commands whose Computer holds a revoked
// Secret binding. Every candidate is compared again under its immutable
// Computer and lease locks. A failing candidate is reported and skipped, so a
// persistently failing Command costs one attempt per pass instead of blocking
// the Commands behind it. The returned position continues the pass; more is
// false when the pass reached the end of the candidates.
func RecoverBatch(ctx context.Context, database db.TxDB, after RecoveryPosition, limit int) (RecoveryPosition, bool, error) {
	if limit <= 0 {
		return after, false, errors.New("command recovery batch limit must be positive")
	}
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id,c.computer_id,c.revision,c.updated_at FROM commands c JOIN environments e ON e.id=c.environment_id
 LEFT JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,c.computer_lease_epoch)
 WHERE ((c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL AND (l.fenced_at IS NOT NULL OR (c.terminal_at IS NULL AND (l.status IN ('lost','released') OR l.expires_at<=clock_timestamp()))))
 OR (c.terminal_at IS NULL AND c.cancel_requested_at IS NULL AND EXISTS(SELECT 1 FROM secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id) WHERE b.environment_id=c.environment_id AND b.computer_id=c.computer_id AND s.status='revoked')))
 AND (c.updated_at,c.id)>($2::timestamptz,$3::uuid)
 ORDER BY c.updated_at,c.id LIMIT $1`, limit, after.UpdatedAt, after.ID)
	if err != nil {
		return after, false, err
	}
	type candidate struct {
		RecoveryCandidate
		updatedAt time.Time
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.EnvironmentID, &c.CommandID, &c.ComputerID, &c.ExpectedRevision, &c.updatedAt)
		return c, err
	})
	if err != nil {
		return after, false, err
	}
	var failures []error
	for _, c := range candidates {
		after = RecoveryPosition{UpdatedAt: c.updatedAt, ID: c.CommandID}
		if err := Recover(ctx, database, c.RecoveryCandidate); err != nil && !errors.Is(err, ErrChanged) {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			return after, true, errors.Join(append(failures, ctx.Err())...)
		}
	}
	more := len(candidates) == limit
	if !more {
		after = RecoveryPosition{}
	}
	return after, more, errors.Join(failures...)
}
