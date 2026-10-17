package slack

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// The installation lock is held by the delivery owner. Provider Retry-After is
// method-wide; routine posting and status pacing are independent channel lanes.
func reserveDelivery(ctx context.Context, tx pgx.Tx, installation uuid.UUID, channel, method string, final bool) (bool, error) {
	var limited bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_method_limits WHERE installation_id=$1 AND method IN ($2,'conversations.info') AND retry_after>clock_timestamp())`, installation, method).Scan(&limited); err != nil || limited {
		return false, err
	}
	if final {
		return true, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO slack_delivery_limits(installation_id,channel_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, installation, channel); err != nil {
		return false, err
	}
	if method == "agents.sessions.setStatus" {
		tag, err := tx.Exec(ctx, `UPDATE slack_delivery_limits SET next_status_at=clock_timestamp()+interval '1 second' WHERE installation_id=$1 AND channel_id=$2 AND next_status_at<=clock_timestamp()`, installation, channel)
		return tag.RowsAffected() == 1, err
	}
	tag, err := tx.Exec(ctx, `UPDATE slack_delivery_limits SET next_post_at=clock_timestamp()+interval '1 second' WHERE installation_id=$1 AND channel_id=$2 AND next_post_at<=clock_timestamp()`, installation, channel)
	return tag.RowsAffected() == 1, err
}

func recordMethodBackoff(ctx context.Context, tx pgx.Tx, installation uuid.UUID, method string, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Minute
	}
	_, err := tx.Exec(ctx, `INSERT INTO slack_method_limits(installation_id,method,retry_after) VALUES($1,$2,clock_timestamp()+$3*interval '1 second') ON CONFLICT(installation_id,method) DO UPDATE SET retry_after=GREATEST(slack_method_limits.retry_after,EXCLUDED.retry_after)`, installation, method, delay.Seconds())
	return err
}
