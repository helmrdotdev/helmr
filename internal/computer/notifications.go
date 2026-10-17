package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

func notifyComputerChanged(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
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

func notifyImageBuildChanged(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	var host *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT worker_host_id FROM image_builds WHERE environment_id=$1 AND id=$2`, env, id).Scan(&host); err != nil {
		return err
	}
	topics := []string{"dispatch"}
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
	return notifyComputerChanged(ctx, tx, env, computer)
}
