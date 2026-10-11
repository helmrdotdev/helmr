package command

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const pendingTimeout = 10 * time.Minute

// ExpirePending preserves the bounded wait for an ordinary command to acquire a
// physical writer. Its execution timeout starts only after delivery, separately.
func ExpirePending(ctx context.Context, database db.TxDB, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("command pending batch limit must be positive")
	}
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id,c.revision FROM computer_commands c JOIN environments e ON e.id=c.environment_id
 WHERE c.status='pending' AND c.computer_lease_epoch IS NULL AND c.created_at<=clock_timestamp()-$1*interval '1 millisecond'
 ORDER BY c.created_at,c.id LIMIT $2`, pendingTimeout.Milliseconds(), limit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Pending, error) {
		var p Pending
		err := row.Scan(&p.EnvironmentID, &p.CommandID, &p.ExpectedRevision)
		return p, err
	})
	if err != nil {
		return 0, err
	}
	for i, p := range candidates {
		err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
			return FailPending(ctx, tx, p, Failure{Code: "computer_command_assignment_timed_out", Detail: []byte(`{"code":"computer_command_assignment_timed_out","retryable":false}`)})
		})
		if err != nil && !errors.Is(err, ErrChanged) {
			return i, err
		}
	}
	return len(candidates), nil
}

// ReconcileCommands is supervised independently of allocation and customer
// Session execution. Persisted pending/lost rows are its restart discovery.
func ReconcileCommands(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		batch, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, recoveryErr := RecoverBatch(batch, database, 256)
		_, pendingErr := ExpirePending(batch, database, 256)
		cancel()
		if err := errors.Join(recoveryErr, pendingErr); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "reconcile Computer commands", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
