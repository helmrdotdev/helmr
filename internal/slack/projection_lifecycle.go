package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// One retained card per bound thread follows execution state changes, including
// checkpoint transitions that produce no authored Session event.
func projectLifecycleCard(ctx context.Context, tx pgx.Tx, o projectionOwner) (bool, error) {
	if o.session != o.front {
		return false, nil
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT thread_ts IS NOT NULL FROM slack_threads WHERE id=$1`, o.thread).Scan(&bound); err != nil {
		return false, err
	}
	if !bound {
		return false, nil
	}
	target := controlTarget{Installation: o.installation, Thread: o.thread, Participant: o.frontSource, Environment: o.environment, Session: o.front}
	state, rawState, err := readLifecycleState(ctx, tx, target)
	if err != nil {
		return false, err
	}
	text, blocks, err := lifecyclePresentationFromState(ctx, tx, o.config, target, "help", "", false, state)
	if err != nil {
		return false, err
	}
	blocks = append(blocks, consoleContext(o.consoleLink(), "Open in Console"))
	body, err := json.Marshal(map[string]any{"text": text, "blocks": blocks, "mrkdwn": false, "parse": "none", "unfurl_links": false, "unfurl_media": false})
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(body)
	tag, err := tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,'execution-controls','controls',$6,$7,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,post_key,continuation_ordinal) DO UPDATE SET payload=EXCLUDED.payload,payload_digest=EXCLUDED.payload_digest,desired_revision=slack_posts.desired_revision+1,status=CASE WHEN slack_posts.status='posted' THEN 'pending' ELSE slack_posts.status END
 WHERE slack_posts.payload_digest<>EXCLUDED.payload_digest AND slack_posts.suppressed_revision=0 AND slack_posts.status NOT IN ('failed','suppressed')`, uuid.NewV7(), o.environment, o.front, o.frontSource, o.thread, body, digest[:])
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE slack_threads SET projected_lifecycle_state=$2 WHERE id=$1`, o.thread, rawState)
	return tag.RowsAffected() > 0, err
}
