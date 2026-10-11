package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type computerLifecyclePosition struct {
	environment, computer uuid.UUID
	epoch                 int64
}

// RunComputerLifecycle settles expired execution authority independently of
// worker connectivity. Its process owner cancels and joins the scan.
func RunComputerLifecycle(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("computer lifecycle database and logger are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var position computerLifecyclePosition
	for {
		next, more, err := reconcileComputerLifecycle(ctx, database, position)
		position = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Computer lifecycle reconciliation failed", "error", err)
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

func reconcileComputerLifecycle(ctx context.Context, database db.TxDB, after computerLifecyclePosition) (computerLifecyclePosition, bool, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Separate time and host-loss ranges so healthy leases need no per-row host lookup.
	// Selection time is only a hint; expiry is rechecked under the ownership locks.
	rows, err := database.Query(scanCtx, `WITH candidates AS MATERIALIZED (
 SELECT l.environment_id,l.computer_id,l.epoch FROM computer_leases l
 WHERE l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing') AND l.expires_at<=statement_timestamp()
 UNION ALL
 SELECT l.environment_id,l.computer_id,l.epoch FROM worker_hosts h JOIN computer_leases l ON l.worker_host_id=h.id
 WHERE l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing') AND (h.current_epoch IS NULL OR h.status NOT IN ('active','draining'))
 UNION ALL
 SELECT l.environment_id,l.computer_id,l.epoch FROM worker_hosts h JOIN computer_leases l ON l.worker_host_id=h.id AND l.worker_epoch<h.current_epoch
 WHERE l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing')
 UNION ALL
 SELECT l.environment_id,l.computer_id,l.epoch FROM worker_hosts h JOIN computer_leases l ON l.worker_host_id=h.id AND l.worker_epoch>h.current_epoch
 WHERE l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing')
 ) SELECT DISTINCT environment_id,computer_id,epoch FROM candidates
 WHERE (environment_id,computer_id,epoch)>($1,$2,$3) ORDER BY environment_id,computer_id,epoch LIMIT 100`, after.environment, after.computer, after.epoch)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (computerLifecyclePosition, error) {
		var p computerLifecyclePosition
		err := row.Scan(&p.environment, &p.computer, &p.epoch)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	if len(candidates) == 0 {
		return computerLifecyclePosition{}, false, nil
	}
	var failures []error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		after = candidate
		if err := expireComputerLease(ctx, database, candidate.environment, candidate.computer, candidate.epoch); err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100 || after != candidates[len(candidates)-1]
	if !more {
		after = computerLifecyclePosition{}
	}
	return after, more, errors.Join(failures...)
}
