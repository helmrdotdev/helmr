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

type schedulePosition struct{ environment, id uuid.UUID }

func RunSchedules(ctx context.Context, database db.TxDB, trust ComputerTrustIssuer, log *slog.Logger) error {
	if database == nil || log == nil {
		return ErrInvalidInput
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var after schedulePosition
	for {
		next, more, err := reconcileSchedules(ctx, database, trust, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "schedule reconciliation failed", "error", err)
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
func reconcileSchedules(ctx context.Context, database db.TxDB, trust ComputerTrustIssuer, after schedulePosition) (schedulePosition, bool, error) {
	scan, stop := context.WithTimeout(ctx, 10*time.Second)
	rows, err := database.Query(scan, `SELECT s.environment_id,s.id FROM agent_schedules s JOIN environments e ON e.id=s.environment_id AND e.retired_at IS NULL
 WHERE s.next_fire_at<=statement_timestamp() AND (s.active_until IS NULL OR s.next_fire_at<s.active_until)
 AND (s.environment_id,s.id)>($1,$2) ORDER BY s.environment_id,s.id LIMIT 100`, after.environment, after.id)
	if err != nil {
		stop()
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (schedulePosition, error) {
		var p schedulePosition
		err := row.Scan(&p.environment, &p.id)
		return p, err
	})
	stop()
	if err != nil {
		return after, false, err
	}
	var failures []error
	for _, p := range candidates {
		if ctx.Err() != nil {
			return after, false, ctx.Err()
		}
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := EvaluateSchedule(attempt, database, trust, p.environment, p.id)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
		after = p
	}
	more := len(candidates) == 100
	if !more {
		after = schedulePosition{}
	}
	return after, more, errors.Join(failures...)
}
