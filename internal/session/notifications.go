package session

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

func notifySessionReaders(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return dbnotify.Notify(ctx, tx, "session:"+id.String(), "slack_projection")
}

// notifySessionsChanged wakes readers and execution discovery in the transaction
// that changed the Sessions. Output-only events use notifySessionReaders instead.
func notifySessionsChanged(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids ...uuid.UUID) error {
	topics := []string{"dispatch", "slack_projection"}
	for _, id := range ids {
		topics = append(topics, "session:"+id.String())
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT l.worker_host_id FROM sessions s
 JOIN computer_leases l ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id)
 WHERE s.environment_id=$1 AND s.id=ANY($2::uuid[]) AND l.fenced_at IS NULL`, env, ids)
	if err != nil {
		return err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, host := range hosts {
		topics = append(topics, "worker:"+host.String())
	}
	return dbnotify.Notify(ctx, tx, topics...)
}
