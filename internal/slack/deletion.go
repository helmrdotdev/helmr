package slack

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// observeDeletion is reached only through an authenticated Events API callback.
// The channel and native message identity select already-known projections. A
// missing message never establishes deletion and never permits a replacement.
func (h EventHandler) observeDeletion(ctx context.Context, team, channel, timestamp string) error {
	if team == "" || channel == "" || !timestampPattern.MatchString(timestamp) {
		return nil
	}
	type owner struct{ installation, thread uuid.UUID }
	var owners []owner
	err := db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT c.installation_id,t.id FROM slack_threads t
 JOIN slack_channels c ON c.id=t.channel_id JOIN slack_installations i ON i.id=c.installation_id
 WHERE i.app_id=$1 AND i.team_id=$2 AND c.slack_channel_id=$3 AND
 (t.thread_ts=$4 OR EXISTS(SELECT 1 FROM slack_posts p JOIN slack_thread_sources s ON s.id=p.thread_source_id WHERE s.thread_id=t.id AND p.message_ts=$4))
 ORDER BY c.installation_id,t.id`, h.AppID, team, channel, timestamp)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o owner
			if err := rows.Scan(&o.installation, &o.thread); err != nil {
				return err
			}
			owners = append(owners, o)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, o := range owners {
		err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, o.installation).Scan(&id); err != nil {
				return err
			}
			var root bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE(thread_ts=$2,false) FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, o.thread, timestamp).Scan(&root); err != nil {
				return err
			}
			if root {
				if _, err := tx.Exec(ctx, `UPDATE slack_threads SET deleted_at=COALESCE(deleted_at,clock_timestamp()),delivery_error='root_deleted',status_confirmation='unknown',stream_status_repair=false,
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,claimed_until=NULL WHERE id=$1`, o.thread); err != nil {
					return err
				}
				// Keep issued evidence while disposing every newer unsent revision.
				// A later positive observation may confirm the issued revision,
				// but cannot requeue content under this deleted root.
				if _, err := tx.Exec(ctx, `UPDATE slack_posts p SET closed_at=COALESCE(p.closed_at,clock_timestamp()),
 suppressed_revision=CASE WHEN p.status IN ('pending','sending','uncertain') THEN p.desired_revision ELSE p.suppressed_revision END,
 status=CASE WHEN p.status='pending' THEN 'suppressed' ELSE p.status END,
 error=CASE WHEN p.status='pending' THEN 'root_deleted' ELSE p.error END,
 inflight_method=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_method END,
 inflight_payload=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_payload END,
 inflight_digest=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_digest END,
 inflight_revision=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_revision END,
 inflight_attempt_id=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_attempt_id END,
 inflight_stream_text=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_stream_text END
 FROM slack_thread_sources s WHERE p.thread_source_id=s.id AND s.thread_id=$1`, o.thread); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `UPDATE slack_posts p SET status='failed',stream_state=CASE WHEN presentation_path='stream' THEN 'stopped' ELSE stream_state END,error='message_deleted',claimed_until=NULL,closed_at=COALESCE(closed_at,clock_timestamp()),
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL
 FROM slack_thread_sources s WHERE p.thread_source_id=s.id AND s.thread_id=$1 AND p.message_ts=$2`, o.thread, timestamp)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
