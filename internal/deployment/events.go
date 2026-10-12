package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// EventRetention is how long Deployment events stay readable. Events are
// retained in PostgreSQL; the retention sweep deletes older rows in batches.
const EventRetention = 90 * 24 * time.Hour

const eventRetentionBatch = 2500

// appendEvent records one Deployment event inside the caller's transaction.
func appendEvent(ctx context.Context, q db.DBTX, env, deployment uuid.UUID, kind, message string) error {
	_, err := q.Exec(ctx, `INSERT INTO deployment_events(environment_id,deployment_id,kind,message) VALUES($1,$2,$3,$4)`, env, deployment, kind, message)
	return err
}

// Event is one retained Deployment lifecycle event.
type Event struct {
	ID         int64
	Kind       string
	Message    string
	Payload    json.RawMessage
	AcceptedAt time.Time
}

// ListEvents returns Deployment events after the given event ID in acceptance
// order, at most limit rows, for a Deployment the principal may read.
func ListEvents(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, deploymentID uuid.UUID, after int64, limit int32) ([]Event, error) {
	record, err := Get(ctx, q, principal, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id,kind,message,payload,accepted_at FROM deployment_events WHERE environment_id=$1 AND deployment_id=$2 AND id>$3 ORDER BY id LIMIT $4`, record.EnvironmentID, record.ID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Kind, &event.Message, &event.Payload, &event.AcceptedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// RunEventRetention deletes Deployment events older than EventRetention.
func RunEventRetention(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		deleted, err := sweepEvents(ctx, database, EventRetention)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Deployment event retention failed", "error", err)
		}
		if deleted == eventRetentionBatch {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func sweepEvents(ctx context.Context, database db.TxDB, retention time.Duration) (int64, error) {
	var deleted int64
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `WITH expired AS (SELECT id FROM deployment_events WHERE accepted_at<statement_timestamp()-$1::interval ORDER BY accepted_at,id LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM deployment_events USING expired WHERE deployment_events.id=expired.id`, retention, eventRetentionBatch)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if errors.Is(err, context.Canceled) {
		return deleted, err
	}
	return deleted, err
}
