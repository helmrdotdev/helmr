package slack

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type deliveryCandidate struct {
	id   uuid.UUID
	kind string
}

// Delivery discovery confers no send authority. Each reconciler locks and rechecks
// its durable owner before freezing a mutation or reserving a history read.
// Omit dependency-blocked pending mutations, but retain unavailable-route cleanup
// and all uncertain/expired mutation recovery even behind a paused predecessor.
// Cleanup exceptions follow agent.LockSlackChannels and ClaimPost speaker checks.
func deliveryCandidates(ctx context.Context, pool db.TxBeginner, limit int) ([]deliveryCandidate, error) {
	if limit < 1 || limit > 64 {
		return nil, errGestureInvalid
	}
	var candidates []deliveryCandidate
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH candidates AS (
 SELECT p.id,'post' AS kind,CASE WHEN p.status='sending' THEN p.claimed_until ELSE p.next_attempt_at END AS due,p.created_at AS service_age,
 CASE WHEN t.thread_ts IS NULL AND p.role='opening' AND t.front_session_id=p.session_id AND t.opening_publication_key=p.publication_key AND p.continuation_ordinal=0 THEN 1 WHEN p.role='question' THEN 0 ELSE 3 END AS priority
 FROM slack_posts p JOIN slack_thread_sources s ON s.id=p.thread_source_id JOIN slack_threads t ON t.id=s.thread_id
 JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id) JOIN slack_channels c ON c.id=sender.channel_id JOIN slack_installations i ON i.id=c.installation_id JOIN environments e ON e.id=c.environment_id JOIN agent_publications r ON r.id=c.publication_id WHERE (t.deleted_at IS NULL OR p.status IN ('sending','uncertain') OR (p.presentation_path='stream' AND p.stream_state='open' AND p.closed_at IS NOT NULL AND p.message_ts IS NOT NULL)) AND
 ((p.status='pending' AND p.next_attempt_at<=clock_timestamp()) OR
 ((p.status='uncertain' OR (p.status='failed' AND p.delivery_disposed_at IS NOT NULL AND p.inflight_attempt_id IS NOT NULL)) AND p.reconciliation_paused_at IS NULL AND p.next_attempt_at<=clock_timestamp()) OR (p.status='sending' AND p.claimed_until<=clock_timestamp()) OR
 (p.presentation_path='stream' AND p.stream_state='open' AND p.closed_at IS NOT NULL AND p.status IN ('posted','suppressed','failed') AND p.next_attempt_at<=clock_timestamp()))
 AND (p.status<>'pending' OR r.revoked_at IS NOT NULL OR i.disconnected_at IS NOT NULL OR i.authorization_lost_at IS NOT NULL OR i.organization_id<>e.org_id OR NOT `+blockingPostPredecessor+`)
 UNION ALL
 SELECT id,'status',next_attempt_at,CASE WHEN desired_revision<>confirmed_revision OR status_confirmation='unknown' THEN refresh_at-interval '30 seconds' ELSE refresh_at END,2 FROM slack_threads WHERE thread_ts IS NOT NULL AND deleted_at IS NULL AND next_attempt_at<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())
 ), ranked AS (SELECT id,kind,due,service_age,priority,row_number() OVER (PARTITION BY priority ORDER BY due,id) AS position FROM candidates),
 shortlist AS (SELECT * FROM ranked ORDER BY position,priority,due,id,kind LIMIT $1)
 SELECT id,kind FROM shortlist ORDER BY LEAST(priority,2),service_age,due,id,kind`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate deliveryCandidate
			if err := rows.Scan(&candidate.id, &candidate.kind); err != nil {
				return err
			}
			candidates = append(candidates, candidate)
		}
		return rows.Err()
	})
	return candidates, err
}

// ReconcileDelivery advances a bounded mix of content, uncertain delivery reads,
// and absolute native status. A blocked old root must not monopolize discovery.
// Discovery interleaves roots, questions, status and other content so no one
// category can monopolize a production batch. Within that shortlist, questions
// and roots lead; ordinary content and status compete by publication/refresh age.
// Successful status service advances refresh_at, while skipped content keeps its
// age. Unconfirmed status leads its refresh deadline by FinishStatus's 30-second
// interval; stream repair gets the same head start. Periodic refresh keeps its deadline.
// Existing next-attempt fields rotate skipped rows without adding a second claim;
// a crash before rotation merely repeats safe claim/reconciliation checks.
func ReconcileDelivery(ctx context.Context, pool db.TxBeginner, client *WebClient, limit int) (int, error) {
	candidates, err := deliveryCandidates(ctx, pool, limit)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if candidate.kind == "status" {
			_, err = ReconcileStatus(ctx, pool, client, candidate.id)
		} else {
			_, err = ReconcilePost(ctx, pool, client, candidate.id)
			if err == nil {
				_, err = ReconcileUnknownPost(ctx, pool, client, candidate.id)
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
		// Never shorten a native Retry-After or a history reservation, and never
		// modify an in-flight mutation's deadline. Claims remain the sole send fence.
		err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
			query := `UPDATE slack_posts SET next_attempt_at=clock_timestamp()+interval '3 seconds' WHERE id=$1 AND (status IN ('pending','uncertain') OR (presentation_path='stream' AND stream_state='open' AND closed_at IS NOT NULL AND status IN ('posted','suppressed','failed'))) AND next_attempt_at<=clock_timestamp()`
			if candidate.kind == "status" {
				query = `UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '3 seconds' WHERE id=$1 AND next_attempt_at<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())`
			}
			_, err := tx.Exec(ctx, query, candidate.id)
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return len(candidates), errors.Join(failures...)
}
