package computer

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

func notifyComputerChanged(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	if err := notifyComputerReaders(ctx, tx, env, id); err != nil {
		return err
	}
	return dbnotify.Notify(ctx, tx, "capacity")
}

func notifyComputerReaders(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT worker_host_id FROM computer_leases
 WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL`, env, id)
	if err != nil {
		return err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	topics := []string{"dispatch"}
	for _, host := range hosts {
		topics = append(topics, "worker:"+host.String())
	}
	return dbnotify.Notify(ctx, tx, topics...)
}

// NotifyCommandReaders wakes the lease holder for new work. An unallocated
// Computer or a retained checkpoint source also needs placement reconciliation.
func NotifyCommandReaders(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	var host uuid.UUID
	var retained bool
	err := tx.QueryRow(ctx, `SELECT l.worker_host_id,`+leaseHasReadyCheckpointSQL+`
 FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.fenced_at IS NULL`, env, id).Scan(&host, &retained)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbnotify.Notify(ctx, tx, "dispatch")
	}
	if err != nil {
		return err
	}
	worker := "worker:" + host.String()
	if retained {
		return dbnotify.Notify(ctx, tx, "dispatch", worker)
	}
	return dbnotify.Notify(ctx, tx, worker)
}

func notifyImageBuildChanged(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	var host *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT worker_host_id FROM image_builds WHERE environment_id=$1 AND id=$2`, env, id).Scan(&host); err != nil {
		return err
	}
	topics := []string{"dispatch", "capacity"}
	if host != nil {
		topics = append(topics, "worker:"+host.String())
	}
	return dbnotify.Notify(ctx, tx, topics...)
}

func notifySaveChanged(ctx context.Context, tx pgx.Tx, env, save uuid.UUID) error {
	var computer uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM computer_saves WHERE environment_id=$1 AND id=$2`, env, save).Scan(&computer); err != nil {
		return err
	}
	return notifyComputerReaders(ctx, tx, env, computer)
}
