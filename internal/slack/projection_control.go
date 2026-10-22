package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// Control acknowledgements need no authored output event. Refresh their existing
// cards and causal Stop notice without inventing a new public execution event.
func projectControlState(ctx context.Context, tx pgx.Tx, o projectionOwner, authorized time.Time) (bool, error) {
	if err := projectActivity(ctx, tx, o, authorized); err != nil {
		return false, err
	}
	lifecycle, err := projectLifecycleCard(ctx, tx, o)
	if err != nil {
		return false, err
	}
	notice, err := projectStopNotices(ctx, tx, o, authorized)
	if err != nil {
		return false, err
	}
	advanced, err := slackroute.AdvanceControlProjection(ctx, tx, o.participant, o.environment, o.session)
	return lifecycle || notice || advanced, err
}

func projectStopNotices(ctx context.Context, tx pgx.Tx, o projectionOwner, authorized time.Time) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT c.id FROM slack_requests r JOIN session_controls c ON (c.environment_id,c.session_id,c.id)=(r.environment_id,r.session_id,r.control_id)
 WHERE r.thread_id=$1 AND r.status='accepted' AND r.operation='native_stop' AND r.source_occurred_at>=$2
 AND NOT EXISTS(SELECT 1 FROM slack_posts posted WHERE posted.thread_id=r.thread_id AND posted.post_key='stopped:'||c.id::text)
 AND NOT EXISTS(SELECT 1 FROM sessions owned JOIN session_processes p ON (p.environment_id,p.session_id)=(owned.environment_id,owned.id)
 WHERE owned.environment_id=r.environment_id AND owned.root_session_id=r.session_id AND p.fenced_at IS NULL AND
 (p.status IN ('lost','stopping','starting') OR p.stop_deadline_at IS NOT NULL OR p.control_error IS NOT NULL OR p.control_acknowledged_at IS NULL OR p.control_acknowledged_at<c.accepted_at OR p.control_attachment<>p.attachment_sequence))`, o.thread, authorized)
	if err != nil {
		return false, err
	}
	controls, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return false, err
	}
	owner := o
	owner.session = o.front
	owner.participant = o.frontSource
	body, _ := json.Marshal(map[string]any{"text": "The work you stopped has ceased.", "mrkdwn": false, "parse": "none", "blocks": []any{map[string]any{"type": "section", "text": plainText("The work you stopped has ceased.")}, consoleContext(owner.consoleLink(), "View in Console")}})
	digest := sha256.Sum256(body)
	changed := false
	for _, control := range controls {
		tag, err := tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,'lifecycle',$7,$8,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,post_key,continuation_ordinal) DO NOTHING`, uuid.NewV7(), o.environment, o.front, o.frontSource, o.thread, "stopped:"+control.String(), body, digest[:])
		if err != nil {
			return false, err
		}
		changed = changed || tag.RowsAffected() > 0
	}
	return changed, nil
}
