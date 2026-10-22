package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// RunExecutionAdmissions settles queued work whose accepted provisioning period
// expired. No automatic new episode is created by the unchanged queued work.
func RunExecutionAdmissions(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ReconcileExecutionAdmissions(ctx, database); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "execution admission reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ReconcileExecutionAdmissions(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT c.environment_id,c.id FROM computers c WHERE c.execution_deadline_at<=clock_timestamp()
 AND `+computer.RunnableExecutionDemandSQL+`
 ORDER BY c.execution_deadline_at,c.environment_id,c.id LIMIT 100`)
	if err != nil {
		return err
	}
	type target struct{ env, id uuid.UUID }
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (target, error) {
		var t target
		err := row.Scan(&t.env, &t.id)
		return t, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, t := range targets {
		if err := expireExecutionAdmission(ctx, database, t.env, t.id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func expireExecutionAdmission(ctx context.Context, database db.TxBeginner, env, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		if err := workergroup.LockExecutionSupply(ctx, tx, env); err != nil {
			return err
		}
		if _, err := org.LockAdmission(ctx, tx, env); err != nil {
			return err
		}
		_, err := session.LockComputerMembers(ctx, tx, env, id)
		if err != nil {
			return err
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE(execution_deadline_at<=clock_timestamp(),false) FROM computers WHERE environment_id=$1 AND id=$2`, env, id).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		if err = session.FailQueuedExecutionTurns(ctx, tx, env, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,revision FROM commands WHERE environment_id=$1 AND computer_id=$2 AND status='pending' AND computer_lease_epoch IS NULL ORDER BY id`, env, id)
		if err != nil {
			return err
		}
		commands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (command.Pending, error) {
			p := command.Pending{EnvironmentID: env}
			err := row.Scan(&p.CommandID, &p.ExpectedRevision)
			return p, err
		})
		if err != nil {
			return err
		}
		for _, pending := range commands {
			if err = command.FailPending(ctx, tx, pending, command.Failure{Code: "execution_deadline_exceeded", Detail: []byte(`{"code":"execution_deadline_exceeded"}`)}); err != nil {
				return err
			}
		}
		if err = computer.FailExecutionAdmission(ctx, tx, env, id); err != nil {
			return err
		}
		return computer.RetireUnusedExecutionAdmission(ctx, tx, env, id)
	})
}
