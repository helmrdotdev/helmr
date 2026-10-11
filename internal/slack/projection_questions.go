package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

func projectQuestionState(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	var source struct {
		Ask uuid.UUID `json:"ask_id"`
	}
	if json.Unmarshal(e.data, &source) != nil || source.Ask == uuid.Nil() {
		return errors.New("question update has no exact ask")
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT status FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND id=$3`, o.environment, o.session, source.Ask).Scan(&state); err != nil {
		return err
	}
	label := map[string]string{"responded": "Question answered.", "cancelled": "Question withdrawn."}[state]
	if label == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,payload FROM slack_posts WHERE thread_source_id=$1 AND ask_id=$2 AND suppressed_revision=0 AND status NOT IN ('failed','suppressed') ORDER BY seq FOR NO KEY UPDATE`, o.participant, source.Ask)
	if err != nil {
		return err
	}
	type post struct {
		id   uuid.UUID
		body []byte
	}
	posts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (post, error) { var p post; err := row.Scan(&p.id, &p.body); return p, err })
	if err != nil {
		return err
	}
	for _, p := range posts {
		var body map[string]json.RawMessage
		if err = json.Unmarshal(p.body, &body); err != nil {
			return err
		}
		var blocks []map[string]json.RawMessage
		if err = json.Unmarshal(body["blocks"], &blocks); err != nil {
			return err
		}
		keptBlocks := blocks[:0]
		for _, block := range blocks {
			var kind, blockID string
			_ = json.Unmarshal(block["type"], &kind)
			_ = json.Unmarshal(block["block_id"], &blockID)
			if blockID == "helmr_answer_cli" {
				continue
			}
			if kind != "actions" {
				keptBlocks = append(keptBlocks, block)
				continue
			}
			var actions []map[string]json.RawMessage
			if err = json.Unmarshal(block["elements"], &actions); err != nil {
				return err
			}
			kept := actions[:0]
			for _, action := range actions {
				var id string
				_ = json.Unmarshal(action["action_id"], &id)
				if id != "helmr.open_answer" {
					kept = append(kept, action)
				}
			}
			if len(kept) > 0 {
				block["elements"], _ = json.Marshal(kept)
				keptBlocks = append(keptBlocks, block)
			}
		}
		stateBlock, _ := json.Marshal(map[string]any{"type": "context", "elements": []any{plainText(label)}})
		var block map[string]json.RawMessage
		_ = json.Unmarshal(stateBlock, &block)
		blocks = append(keptBlocks, block)
		body["blocks"], _ = json.Marshal(blocks)
		var text string
		_ = json.Unmarshal(body["text"], &text)
		body["text"], _ = json.Marshal(text + "\n" + label)
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if _, err = tx.Exec(ctx, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,status=CASE WHEN status='posted' THEN 'pending' ELSE status END WHERE id=$1`, p.id, raw, digest[:]); err != nil {
			return err
		}
	}
	return nil
}
