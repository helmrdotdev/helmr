package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// preparationStateSQL samples the front Session's oldest unfinished Turn and
// why it has not started: its Computer is starting, waking or waiting for
// storage. A Turn that runs, would run at once on an awake Computer, or waits
// on another hold that Slack status reports as suspended, is ready. The Turn status is part of the sample so a card still pending settles
// when its Turn starts or ends. Alias s is the front Session.
const preparationStateSQL = `(SELECT jsonb_build_object('turn',w.id,'status',w.status,'phase',CASE
 WHEN w.status='running' THEN 'ready'
 WHEN EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id AND h.kind<>'storage_pressure' AND h.released_at IS NULL) THEN 'ready'
 WHEN EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id AND h.kind='storage_pressure' AND h.released_at IS NULL) THEN 'storage'
 WHEN EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.status='active' AND l.fenced_at IS NULL) THEN 'ready'
 WHEN EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=s.environment_id AND cp.computer_id=s.computer_id AND cp.status IN ('capturing','sealed','ready','restoring','aborting')) THEN 'waking'
 ELSE 'starting' END)
 FROM turns w WHERE w.environment_id=s.environment_id AND w.session_id=s.id AND w.status IN ('queued','running') ORDER BY w.created_at,w.id LIMIT 1)`

type preparationState struct {
	Turn  *uuid.UUID `json:"turn"`
	Phase string     `json:"phase"`
}

// projectPreparation keeps one preparation card per Turn that had to wait for
// its Computer. The card appears only when the Turn first waits and stays
// pending until the Turn starts or ends; a Turn on an awake Computer gets none.
func projectPreparation(ctx context.Context, tx pgx.Tx, o projectionOwner) (bool, error) {
	if o.session != o.front {
		return false, nil
	}
	var bound, same bool
	var currentRaw []byte
	if err := tx.QueryRow(ctx, `SELECT t.thread_ts IS NOT NULL,t.projected_preparation IS NOT DISTINCT FROM `+preparationStateSQL+`,`+preparationStateSQL+`
 FROM slack_threads t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) WHERE t.id=$1`, o.thread).Scan(&bound, &same, &currentRaw); err != nil {
		return false, err
	}
	if !bound || same {
		return false, nil
	}
	var current preparationState
	if currentRaw != nil {
		if err := json.Unmarshal(currentRaw, &current); err != nil {
			return false, err
		}
	}
	changed := false
	if current.Turn != nil && current.Phase != "ready" {
		posted, err := upsertPreparationCard(ctx, tx, o, *current.Turn, preparationTitle(current.Phase), "in_progress")
		if err != nil {
			return false, err
		}
		changed = posted
	}
	rows, err := tx.Query(ctx, `SELECT w.id,CASE WHEN w.started_at IS NOT NULL THEN 'started' ELSE w.status END FROM slack_posts p JOIN turns w ON (w.environment_id,w.session_id,w.id)=(p.environment_id,p.session_id,p.turn_id)
 WHERE p.thread_source_id=$1 AND p.role='preparation' AND p.payload IS NOT NULL AND convert_from(p.payload,'UTF8')::jsonb#>>'{blocks,0,status}'='in_progress' AND w.status<>'queued'`, o.frontSource)
	if err != nil {
		return false, err
	}
	type settled struct {
		turn   uuid.UUID
		status string
	}
	turns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (settled, error) {
		var s settled
		err := row.Scan(&s.turn, &s.status)
		return s, err
	})
	if err != nil {
		return false, err
	}
	for _, turn := range turns {
		title, state := "Computer ready", "complete"
		switch turn.status {
		case "failed":
			title, state = "Could not prepare the Computer", "error"
		case "cancelled", "interrupted":
			title, state = "Stopped", "error"
		}
		posted, err := upsertPreparationCard(ctx, tx, o, turn.turn, title, state)
		if err != nil {
			return false, err
		}
		changed = changed || posted
	}
	_, err = tx.Exec(ctx, `UPDATE slack_threads SET projected_preparation=$2 WHERE id=$1`, o.thread, currentRaw)
	return changed, err
}

func preparationTitle(phase string) string {
	switch phase {
	case "storage":
		return "Waiting for storage space"
	case "waking":
		return "Waking the Computer"
	default:
		return "Starting the Computer"
	}
}

func upsertPreparationCard(ctx context.Context, tx pgx.Tx, o projectionOwner, turn uuid.UUID, title, state string) (bool, error) {
	// Slack replaces a block only when its block_id changes.
	card := map[string]any{"type": "task_card", "task_id": "preparation", "block_id": "preparation:" + state + ":" + title, "title": title, "status": state}
	body, err := json.Marshal(map[string]any{"text": title, "blocks": []any{card}, "mrkdwn": false, "parse": "none", "unfurl_links": false, "unfurl_media": false})
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(body)
	tag, err := tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,'preparation',$7,$8,$9,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,post_key,continuation_ordinal) DO UPDATE SET payload=EXCLUDED.payload,payload_digest=EXCLUDED.payload_digest,desired_revision=slack_posts.desired_revision+1,status=CASE WHEN slack_posts.status='posted' THEN 'pending' ELSE slack_posts.status END
 WHERE slack_posts.payload_digest<>EXCLUDED.payload_digest AND slack_posts.suppressed_revision=0 AND slack_posts.status NOT IN ('failed','suppressed')`, uuid.NewV7(), o.environment, o.front, o.frontSource, o.thread, "preparation:"+turn.String(), turn, body, digest[:])
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
