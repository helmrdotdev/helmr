package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"uuid"
)

func projectWorkOverflow(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	body, err := json.Marshal(map[string]any{"text": "Further work details are available in Console.", "mrkdwn": false, "parse": "none", "blocks": []any{consoleContext(o.consoleLink(), "Continue reading work details in Console")}})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,'intermediate',$7,$8,$9,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,post_key,continuation_ordinal) DO NOTHING`, uuid.NewV7(), o.environment, o.session, o.participant, o.thread, "overflow:"+e.turn.String(), e.turn, body, digest[:])
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()) WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate'`, o.participant, e.turn)
	return err
}
