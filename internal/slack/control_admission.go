package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db"
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
		if json.Unmarshal(raw, &gesture) != nil || !validControl(gesture.Control) || gesture.Control.Target.Installation != installation || (gesture.Control.Action != "stop" && gesture.Control.Action != "answer") {
			return errControlInvalid
		}
		target := gesture.Control.Target
		reject := func(code string) error {
			_, err := tx.Exec(ctx, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error=$2 WHERE id=$1 AND status='received'`, id, code)
			return err
		}
		var channel uuid.UUID
		var channelEnabled time.Time
		err := tx.QueryRow(ctx, `SELECT c.id,pub.created_at FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id AND t.front_session_id=p.session_id JOIN slack_channels c ON c.id=t.channel_id JOIN agent_publications pub ON pub.id=c.publication_id
 WHERE p.id=$1 AND p.thread_id=$2 AND p.environment_id=$3 AND p.session_id=$4 AND c.installation_id=$5 AND t.deleted_at IS NULL`, target.Participant, target.Thread, target.Environment, target.Session, installation).Scan(&channel, &channelEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return reject("target_unavailable")
		}
		if err != nil {
			return err
		}
		eligible, err := agent.LockSlackChannels(ctx, tx, target.Environment, []uuid.UUID{channel})
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
		answering := gesture.Control.Action == "answer"
		if answering {
			opened, err := slackMessageTime(gesture.Control.SourceTimestamp)
			if err != nil || !opened.Equal(source) {
				return reject("control_source_mismatch")
			}
		}
		if answering && gesture.Control.Actor != actor {
			return reject("control_actor_mismatch")
		}
		user, code, err := linkedHuman(ctx, tx, team, actor, target.Environment, source, answering)
		if err != nil {
			return err
		}
		if code != "" {
			return reject(code)
		}
		var askID, controlID *uuid.UUID
		err = db.RunTx(ctx, tx, func(coreTx pgx.Tx) error {
			caller := agent.Caller{Kind: "user", ID: user}
			if answering {
				var question []byte
				if err := coreTx.QueryRow(ctx, `SELECT question FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, target.Environment, target.Session, target.Turn, target.Ask).Scan(&question); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return agent.ErrNotReady
					}
					return err
				}
				if question == nil {
					return agent.ErrNotReady
				}
				value, err := parseFormAnswer(question, gesture.Form)
				if err != nil {
					return err
				}
				answer, err := agent.RespondAsk(ctx, coreTx, caller, agent.AskAnswerRequest{EnvironmentID: target.Environment, SessionID: target.Session, TurnID: target.Turn, AskID: target.Ask, ResponseID: "slack:" + id.String(), Answer: value})
				if err != nil {
					return err
				}
				askID = &answer.ID
				gesture.Receipt, _ = json.Marshal(map[string]any{"ask_id": answer.ID, "status": answer.Status})
			} else {
				receipt, err := agent.ControlSession(ctx, coreTx, caller, agent.SessionControlRequest{EnvironmentID: target.Environment, SessionID: target.Session, Kind: "interrupt", RetryKey: "slack:" + id.String()})
				if err != nil {
					return err
				}
				controlID = &receipt.ID
				gesture.Receipt, _ = json.Marshal(map[string]any{"control_id": receipt.ID, "hold_id": receipt.HoldID})
			}
			var deleted bool
			if err := coreTx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM slack_threads WHERE id=$1 FOR SHARE`, target.Thread).Scan(&deleted); err != nil {
				return err
			}
			if deleted {
				return agent.ErrNotReady
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
			case errors.Is(err, agent.ErrDenied):
				return reject("permission_denied")
			case errors.Is(err, agent.ErrNotReady), errors.Is(err, agent.ErrTerminal):
				return reject("target_unavailable")
			case errors.Is(err, agent.ErrAskAlreadyResponded):
				return reject("ask_already_answered")
			case errors.Is(err, agent.ErrAskCancelled):
				return reject("ask_cancelled")
			case errors.Is(err, agent.ErrConflict):
				return reject("response_conflict")
			case errors.Is(err, conversation.ErrAnswerInvalid), errors.Is(err, conversation.ErrLimit), errors.Is(err, agent.ErrInvalidInput):
				return reject("answer_invalid")
			default:
				return err
			}
		}
		raw, err = json.Marshal(gesture)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$7,ask_id=$8,control_id=$9,payload=$10 WHERE id=$1`, id, user, target.Thread, target.Participant, target.Environment, target.Session, gesture.Control.Action, askID, controlID, raw)
		return err
	})
}
