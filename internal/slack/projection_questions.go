package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
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
	var question, answer []byte
	var responder string
	if err := tx.QueryRow(ctx, `SELECT a.status,a.question,a.answer,CASE WHEN a.answered_by_user_id IS NOT NULL THEN COALESCE(u.display_name,'user') WHEN a.answered_by_api_key_id IS NOT NULL THEN 'API key' ELSE '' END FROM turn_asks a LEFT JOIN users u ON u.id=a.answered_by_user_id WHERE a.environment_id=$1 AND a.session_id=$2 AND a.id=$3`, o.environment, o.session, source.Ask).Scan(&state, &question, &answer, &responder); err != nil {
		return err
	}
	label := map[string]string{"answered": "Question answered.", "cancelled": "Question withdrawn."}[state]
	if state == "answered" {
		name := []rune(responder)
		if len(name) > 80 {
			name = name[:80]
		}
		label = "Answered by " + string(name) + "."
		if question != nil && answer != nil {
			q, err := parseFormQuestion(question)
			if err != nil {
				return err
			}
			if q.Type == "choice" {
				var resolved struct {
					Selected []struct {
						ID string `json:"id"`
					} `json:"selected"`
				}
				if err := json.Unmarshal(answer, &resolved); err != nil {
					return err
				}
				ids := map[string]bool{}
				for _, item := range resolved.Selected {
					ids[item.ID] = true
				}
				labels := []string{}
				for _, option := range q.Options {
					if ids[option.ID] {
						labels = append(labels, option.Label)
					}
				}
				if len(labels) > 0 {
					label = "Answered by " + string(name) + ": " + strings.Join(labels, ", ")
				}
			}
		}
	}
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
	for index, p := range posts {
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
			var kind string
			_ = json.Unmarshal(block["type"], &kind)
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
				if id != "choice" && id != "helmr.submit_choices" && id != "helmr.open_answer" && !strings.HasPrefix(id, "helmr.choose.") {
					kept = append(kept, action)
				}
			}
			if len(kept) > 0 {
				block["elements"], _ = json.Marshal(kept)
				keptBlocks = append(keptBlocks, block)
			}
		}
		blocks = keptBlocks
		if index == len(posts)-1 {
			content, _ := json.Marshal([]map[string]string{{"type": "text", "text": label}})
			rendered, err := RenderContent([]ContentSource{{Sequence: 1, Content: content}}, false)
			if err != nil {
				return err
			}
			for _, part := range rendered {
				for _, value := range part.Body.Blocks {
					encoded, _ := json.Marshal(value)
					var block map[string]json.RawMessage
					_ = json.Unmarshal(encoded, &block)
					blocks = append(blocks, block)
				}
			}
			var text string
			_ = json.Unmarshal(body["text"], &text)
			body["text"], _ = json.Marshal(text + "\n" + label)
		}
		body["blocks"], _ = json.Marshal(blocks)

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
