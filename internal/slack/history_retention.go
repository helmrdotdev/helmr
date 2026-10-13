package slack

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// HistoryPosition is the last Session visited by a bounded retention scan.
type HistoryPosition struct{ environment, session uuid.UUID }

// RunHistoryRetention is joined by the process root. Disk and diagnostic
// retention have separate owners and are not changed by this policy.
func RunHistoryRetention(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("session history retention requires database and logger")
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var after HistoryPosition
	for {
		next, more, err := ReconcileHistory(ctx, database, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Session history retention failed", "error", err)
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

// Discovery is keyset bounded: an obligated or failing Session cannot exclude
// later owners. Each candidate is rechecked under the normal Session lock order.
func ReconcileHistory(ctx context.Context, database db.TxDB, after HistoryPosition) (HistoryPosition, bool, error) {
	scan, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := database.Query(scan, `SELECT environment_id,id FROM sessions WHERE status IN ('closed','cancelled') AND history_expired_at IS NULL AND (history_eligible_at IS NULL OR history_expires_at<=EXTRACT(epoch FROM clock_timestamp())) AND (environment_id,id)>($1,$2) ORDER BY environment_id,id LIMIT 100`, after.environment, after.session)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (HistoryPosition, error) {
		var p HistoryPosition
		err := row.Scan(&p.environment, &p.session)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	var failures []error
	for _, p := range candidates {
		if ctx.Err() != nil {
			return after, false, ctx.Err()
		}
		after = p
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := RetainSessionHistory(attempt, database, p.environment, p.session)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100
	if !more {
		after = HistoryPosition{}
	}
	return after, more, errors.Join(failures...)
}

// RetainSessionHistory expires a Session and its Slack content atomically.
// Identities and digests remain available for exact retries after expiry.
func RetainSessionHistory(ctx context.Context, database db.TxBeginner, environment, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		expired, err := session.RetainHistory(ctx, tx, environment, id)
		if err != nil || !expired {
			return err
		}
		// Slack mirrors share this Session's selected content-retention boundary.
		// Delivery obligations above must be settled first; keep publication and
		// request identities/digests so expired content never becomes fresh work.
		if _, err := tx.Exec(ctx, `UPDATE slack_requests SET payload=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2 AND status='accepted'`, environment, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET payload=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()),confirmed_stream_text='',
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL,closed_at=COALESCE(closed_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, id); err != nil {
			return err
		}
		return nil
	})
}
