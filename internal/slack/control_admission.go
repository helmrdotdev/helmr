package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

type controlGesture struct {
	Control controlEnvelope `json:"control"`
	Form    formState       `json:"form,omitempty"`
	Receipt json.RawMessage `json:"receipt,omitempty"`
}

// admitControl consumes an already authenticated signed-control gesture. A
// channel-visible token supplies a target, never the human's current authority.
func admitControl(ctx context.Context, pool db.TxBeginner, id uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var installation uuid.UUID
		var raw []byte
		var status string
		if err := tx.QueryRow(ctx, `SELECT installation_id,payload,status FROM slack_requests WHERE id=$1`, id).Scan(&installation, &raw, &status); err != nil {
			return err
		}
		if status != "received" {
			return nil
		}
		var gesture controlGesture
		if json.Unmarshal(raw, &gesture) != nil || !validControl(gesture.Control) || gesture.Control.Target.Installation != installation || (gesture.Control.Action != "answer" && gesture.Control.Action != "choose" && gesture.Control.Action != "submit_choices") {
			return errControlInvalid
		}
		target := gesture.Control.Target
		reject := func(code string) error {
			_, err := tx.Exec(ctx, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error=$2 WHERE id=$1 AND status='received'`, id, code)
			return err
		}
		var channel uuid.UUID
		var channelEnabled time.Time
		var conversation, peer, peerTeam string
		err := tx.QueryRow(ctx, `SELECT c.id,pub.created_at,c.slack_channel_id,COALESCE(t.recipient_user_id,''),COALESCE(t.recipient_team_id,'') FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id AND t.front_session_id=p.session_id JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_connections pub ON pub.id=c.connection_id
 WHERE p.id=$1 AND p.thread_id=$2 AND p.environment_id=$3 AND p.session_id=$4 AND c.installation_id=$5 AND t.deleted_at IS NULL`, target.Participant, target.Thread, target.Environment, target.Session, installation).Scan(&channel, &channelEnabled, &conversation, &peer, &peerTeam)
		if errors.Is(err, pgx.ErrNoRows) {
			return reject("target_unavailable")
		}
		if err != nil {
			return err
		}
		eligible, err := slackroute.LockChannels(ctx, tx, target.Environment, []uuid.UUID{channel})
		if err != nil {
			return err
		}
		var team string
		var active bool
		var authorized, connected time.Time
		if err = tx.QueryRow(ctx, `SELECT team_id,disconnected_at IS NULL AND authorization_lost_at IS NULL,authorized_at,connected_at FROM slack_installations WHERE id=$1`, installation).Scan(&team, &active, &authorized, &connected); err != nil {
			return err
		}
		var actor string
		var source time.Time
		var unexpired bool
		expires := time.Unix(gesture.Control.ExpiresAt, 0).UTC()
		if err = tx.QueryRow(ctx, `SELECT status,slack_user_id,source_occurred_at,expires_at>clock_timestamp() AND $2>clock_timestamp() FROM slack_requests WHERE id=$1 FOR UPDATE`, id, expires).Scan(&status, &actor, &source, &unexpired); err != nil {
			return err
		}
		if status != "received" {
			return nil
		}
		if !active || !eligible {
			return reject("binding_unavailable")
		}
		if source.Before(authorized) || source.Before(connected) || source.Before(channelEnabled) {
			return reject("stale_gesture")
		}
		if !unexpired {
			return reject("gesture_expired")
		}
		if slackroute.IsDirectMessage(conversation) && (peer == "" || peer != actor || peerTeam != team) {
			return reject("permission_denied")
		}
		answering := gesture.Control.Action == "answer" || gesture.Control.Action == "choose" || gesture.Control.Action == "submit_choices"
		if gesture.Control.Action == "answer" {
			opened, err := slackMessageTime(gesture.Control.SourceTimestamp)
			if err != nil || !opened.Equal(source) {
				return reject("control_source_mismatch")
			}
		}
		if gesture.Control.Action == "answer" && gesture.Control.Actor != actor {
			return reject("control_actor_mismatch")
		}
		user, code, err := linkedHuman(ctx, tx, team, actor, target.Environment, source, answering)
		if err != nil {
			return err
		}
		if code != "" {
			return reject(code)
		}
		var askID *uuid.UUID
		err = db.RunTx(ctx, tx, func(coreTx pgx.Tx) error {
			caller := session.Caller{Kind: "user", ID: user}
			if answering {
				var question []byte
				if err := coreTx.QueryRow(ctx, `SELECT question FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, target.Environment, target.Session, target.Turn, target.Ask).Scan(&question); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return session.ErrNotReady
					}
					return err
				}
				if question == nil {
					return session.ErrNotReady
				}
				var value json.RawMessage
				if gesture.Control.Action == "choose" {
					value, _ = json.Marshal(map[string]any{"selectedIds": []string{gesture.Control.OptionID}})
				} else {
					var err error
					if gesture.Control.Action == "submit_choices" {
						value, err = parseInlineAnswer(question, gesture.Form)
					} else {
						value, err = parseFormAnswer(question, gesture.Form)
					}
					if err != nil {
						return err
					}
				}
				answer, err := session.AnswerAsk(ctx, coreTx, caller, session.AskAnswerRequest{EnvironmentID: target.Environment, SessionID: target.Session, TurnID: target.Turn, AskID: target.Ask, AnswerID: "slack:" + id.String(), Answer: value})
				if err != nil {
					return err
				}
				askID = &answer.ID
				gesture.Receipt, _ = json.Marshal(map[string]any{"ask_id": answer.ID, "status": answer.Status})
			}
			var deleted bool
			if err := coreTx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM slack_threads WHERE id=$1 FOR SHARE`, target.Thread).Scan(&deleted); err != nil {
				return err
			}
			if deleted {
				return session.ErrNotReady
			}
			if err := coreTx.QueryRow(ctx, `SELECT expires_at>clock_timestamp() AND $2>clock_timestamp() FROM slack_requests WHERE id=$1`, id, expires).Scan(&unexpired); err != nil {
				return err
			}
			if !unexpired {
				return errAdmissionExpired
			}
			return nil
		})
		if err != nil {
			switch {
			case errors.Is(err, errAdmissionExpired):
				return reject("gesture_expired")
			case (errors.Is(err, session.ErrDenied) || errors.Is(err, computer.ErrDenied)):
				return reject("permission_denied")
			case (errors.Is(err, session.ErrNotReady) || errors.Is(err, computer.ErrNotReady)), (errors.Is(err, session.ErrTerminal) || errors.Is(err, computer.ErrTerminal)):
				return reject("target_unavailable")
			case errors.Is(err, session.ErrAskAlreadyAnswered):
				return reject("ask_already_answered")
			case errors.Is(err, session.ErrAskCancelled):
				return reject("ask_cancelled")
			case (errors.Is(err, session.ErrConflict) || errors.Is(err, computer.ErrConflict)):
				return reject("response_conflict")
			case errors.Is(err, content.ErrAnswerInvalid), errors.Is(err, content.ErrLimit), (errors.Is(err, session.ErrInvalidInput) || errors.Is(err, computer.ErrInvalidInput)):
				return reject("answer_invalid")
			default:
				return err
			}
		}
		operation := gesture.Control.Action
		if answering {
			operation = "answer"
		}
		raw, err = json.Marshal(gesture)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$7,ask_id=$8,control_id=$9,payload=$10 WHERE id=$1`, id, user, target.Thread, target.Participant, target.Environment, target.Session, operation, askID, nil, raw)
		return err
	})
}
