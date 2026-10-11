package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// claimRequests reserves a short scan interval, not a second admission identity.
// A crash leaves the same received row eligible again; admission always locks and
// rechecks that receipt before any core work. No remote request is involved.
func claimRequests(ctx context.Context, pool db.TxBeginner, limit int) ([]uuid.UUID, error) {
	if limit < 1 || limit > 64 {
		return nil, errGestureInvalid
	}
	var ids []uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH due AS (SELECT id FROM slack_requests WHERE status='received' AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,id LIMIT $1 FOR UPDATE SKIP LOCKED)
 UPDATE slack_requests r SET next_attempt_at=clock_timestamp()+interval '30 seconds' FROM due WHERE r.id=due.id RETURNING r.id`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// ReconcileRequest owns completion of one durable input receipt. Success means
// the receipt is terminal, not that an admitted Turn has started or completed.
func ReconcileRequest(ctx context.Context, pool db.TxBeginner, trust agent.ComputerTrustIssuer, config ProjectionConfig, client *WebClient, id uuid.UUID) error {
	if !config.valid() {
		return errGestureInvalid
	}
	var raw []byte
	var status string
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status,payload FROM slack_requests WHERE id=$1`, id).Scan(&status, &raw)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || status != "received" {
		return err
	}
	var payload struct {
		Control   json.RawMessage `json:"control"`
		Timestamp string          `json:"message_ts"`
	}
	if json.Unmarshal(raw, &payload) != nil || (nonnullJSON(payload.Control) == (payload.Timestamp != "")) {
		err = errGestureInvalid
	} else if nonnullJSON(payload.Control) {
		err = admitControl(ctx, pool, id)
	} else {
		err = admitMessage(ctx, pool, trust, client, id)
	}
	if errors.Is(err, errGestureInvalid) || errors.Is(err, errControlInvalid) {
		return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return rejectMessage(ctx, tx, id, "gesture_invalid") })
	}
	return err
}

// ReconcileRequests advances a bounded batch. Operational errors leave received
// rows retryable after the reserved interval; source expiry still prevents late
// admission. One failed request does not prevent independent receipts progressing.
func ReconcileRequests(ctx context.Context, pool db.TxBeginner, trust agent.ComputerTrustIssuer, config ProjectionConfig, client *WebClient, limit int) (int, error) {
	ids, err := claimRequests(ctx, pool, limit)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, id := range ids {
		if err := ReconcileRequest(ctx, pool, trust, config, client, id); err != nil {
			failures = append(failures, err)
		}
	}
	return len(ids), errors.Join(failures...)
}

// RunRequests consumes only authenticated durable receipts. Slack HTTP handlers
// acknowledge persistence; this dispatcher loop owns subsequent core admission.
func RunRequests(ctx context.Context, pool db.TxBeginner, trust agent.ComputerTrustIssuer, config ProjectionConfig, client *WebClient, log *slog.Logger) error {
	if pool == nil || client == nil || log == nil || !config.valid() {
		return errGestureInvalid
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		batch, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := ReconcileRequests(batch, pool, trust, config, client, 64)
		_, feedbackErr := ReconcileRejectedFeedback(batch, pool, client, config, 4)
		err = errors.Join(err, feedbackErr)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Slack request reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
