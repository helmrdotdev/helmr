package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

type nativeStopGesture struct {
	NativeStop bool   `json:"native_stop"`
	Channel    string `json:"channel"`
	Thread     string `json:"thread_ts"`
}

// The signed Events API envelope supplies provenance; no public button token or
// stream timestamp is treated as authority to stop a Session.
func (h EventHandler) receiveNativeStop(ctx context.Context, envelope eventEnvelope) error {
	e := envelope.Event
	if envelope.TeamID == "" || envelope.EventID == "" || e.User == "" || e.BotID != "" || e.AppID != "" || !validConversationID(e.Channel) || !timestampPattern.MatchString(e.Thread) {
		return errGestureInvalid
	}
	occurred, err := slackMessageTime(e.EventTimestamp)
	if err != nil || occurred.After(time.Now().Add(5*time.Minute)) {
		return errGestureInvalid
	}
	raw, _ := json.Marshal(nativeStopGesture{true, e.Channel, e.Thread})
	return db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		var installation uuid.UUID
		var bot string
		err := tx.QueryRow(ctx, `SELECT id,bot_user_id FROM slack_installations WHERE app_id=$1 AND team_id=$2 AND disconnected_at IS NULL FOR KEY SHARE`, h.AppID, envelope.TeamID).Scan(&installation, &bot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if bot == e.User {
			return nil
		}
		receipt, err := receiveGesture(ctx, tx, inboundGesture{Installation: installation, Key: envelope.EventID, Actor: e.User, OccurredAt: occurred, ExpiresAt: occurred.Add(5 * time.Minute), Payload: raw})
		if err != nil || receipt.Status != "received" {
			return err
		}
		// Fill the observation once, in the same transaction as the first receipt.
		// A retry must not acquire a newer front or execution generation.
		tag, err := tx.Exec(ctx, `UPDATE slack_requests r SET thread_id=t.id,environment_id=s.environment_id,session_id=s.id,thread_source_id=src.id,observed_authority_generation=s.authority_generation,observed_next_turn_seq=s.next_turn_seq
 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) JOIN slack_thread_sources src ON src.thread_id=t.id AND src.session_id=s.id
 WHERE r.id=$1 AND r.observed_authority_generation IS NULL AND c.installation_id=$2 AND c.slack_channel_id=$3 AND t.thread_ts=$4 AND t.deleted_at IS NULL
 AND (left(c.slack_channel_id,1)<>'D' OR (t.recipient_user_id=r.slack_user_id AND t.recipient_team_id=c.team_id))
 AND s.created_at<=$5 AND NOT EXISTS(SELECT 1 FROM turns newer WHERE newer.environment_id=s.environment_id AND newer.session_id=s.id AND newer.created_at>$5) AND NOT EXISTS(SELECT 1 FROM session_controls newer WHERE newer.environment_id=s.environment_id AND newer.session_id=s.id AND newer.kind<>'interrupt' AND newer.accepted_at>$5)`, receipt.ID, installation, e.Channel, e.Thread, occurred)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var observed bool
			if err = tx.QueryRow(ctx, `SELECT observed_authority_generation IS NOT NULL FROM slack_requests WHERE id=$1`, receipt.ID).Scan(&observed); err != nil {
				return err
			}
			if !observed {
				return rejectMessage(ctx, tx, receipt.ID, "stale_gesture")
			}
		}
		return nil
	})
}

func admitNativeStop(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, id uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var installation, env, front, thread, source, channel uuid.UUID
		var status, actor, team string
		var occurred, enabled, authorized, connected time.Time
		var observed session.ControlObservation
		err := tx.QueryRow(ctx, `SELECT r.status,r.installation_id,r.environment_id,r.session_id,r.thread_id,r.thread_source_id,r.slack_user_id,r.source_occurred_at,r.observed_authority_generation,r.observed_next_turn_seq,c.id,pub.created_at,i.team_id,i.authorized_at,i.connected_at
 FROM slack_requests r JOIN slack_threads t ON t.id=r.thread_id AND t.front_session_id=r.session_id JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id AND c.installation_id=r.installation_id JOIN slack_connections pub ON pub.id=c.connection_id JOIN slack_installations i ON i.id=r.installation_id WHERE r.id=$1 AND t.deleted_at IS NULL AND (left(c.slack_channel_id,1)<>'D' OR (t.recipient_user_id=r.slack_user_id AND t.recipient_team_id=c.team_id))`, id).Scan(&status, &installation, &env, &front, &thread, &source, &actor, &occurred, &observed.AuthorityGeneration, &observed.NextTurnSequence, &channel, &enabled, &team, &authorized, &connected)
		if errors.Is(err, pgx.ErrNoRows) {
			return rejectMessage(ctx, tx, id, "target_unavailable")
		}
		if err != nil {
			return err
		}
		if status != "received" {
			return nil
		}
		active, err := slackroute.LockChannels(ctx, tx, env, []uuid.UUID{channel})
		if err != nil {
			return err
		}
		// Serialize native Stops with the message path before either takes core locks.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('slack-stop:'||$1::uuid::text,0))`, thread); err != nil {
			return err
		}
		var unexpired bool
		if err = tx.QueryRow(ctx, `SELECT status,expires_at>clock_timestamp() FROM slack_requests WHERE id=$1 FOR UPDATE`, id).Scan(&status, &unexpired); err != nil {
			return err
		}
		if status != "received" {
			return nil
		}
		if !active {
			return rejectMessage(ctx, tx, id, "binding_unavailable")
		}
		// Reload generation timestamps after acquiring the route gate.
		if err = tx.QueryRow(ctx, `SELECT i.authorized_at,i.connected_at,p.created_at FROM slack_installations i JOIN slack_connections p ON p.slack_installation_id=i.id WHERE i.id=$1`, installation).Scan(&authorized, &connected, &enabled); err != nil {
			return err
		}
		if occurred.Before(authorized) || occurred.Before(connected) || occurred.Before(enabled) {
			return rejectMessage(ctx, tx, id, "stale_gesture")
		}
		if !unexpired {
			return rejectMessage(ctx, tx, id, "gesture_expired")
		}
		human, code, err := linkedSessionHuman(ctx, tx, team, actor, installation, env, front, occurred)
		if err != nil {
			return err
		}
		if code != "" {
			return rejectMessage(ctx, tx, id, code)
		}
		var control uuid.UUID
		err = db.RunTx(ctx, tx, func(core pgx.Tx) error {
			err = core.QueryRow(ctx, `SELECT c.id FROM slack_requests r JOIN session_controls c ON (c.environment_id,c.session_id,c.id)=(r.environment_id,r.session_id,r.control_id) JOIN session_holds h ON (h.environment_id,h.id)=(c.environment_id,c.hold_id) JOIN sessions s ON (s.environment_id,s.id)=(r.environment_id,r.session_id)
 WHERE r.thread_id=$1 AND r.status='accepted' AND r.operation='native_stop' AND h.released_at IS NULL AND s.next_turn_seq=$2 AND r.observed_next_turn_seq=$2 ORDER BY r.finished_at LIMIT 1 FOR SHARE OF h`, thread, observed.NextTurnSequence).Scan(&control)
			if errors.Is(err, pgx.ErrNoRows) {
				receipt, controlErr := session.ControlSession(ctx, auditPolicy, core, slackCaller(human, thread), session.SessionControlRequest{EnvironmentID: env, SessionID: front, Kind: "interrupt", IdempotencyKey: "slack:" + id.String(), Observed: &observed, InterruptQueued: true})
				if controlErr != nil {
					return controlErr
				}
				control = receipt.ID
			} else if err != nil {
				return err
			}
			var deleted bool
			if err = core.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM slack_threads WHERE id=$1 FOR SHARE`, thread).Scan(&deleted); err != nil {
				return err
			}
			if deleted {
				return session.ErrNotReady
			}
			if err = core.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&unexpired); err != nil {
				return err
			}
			if !unexpired {
				return errAdmissionExpired
			}
			return nil
		})
		if errors.Is(err, errAdmissionExpired) {
			return rejectMessage(ctx, tx, id, "gesture_expired")
		}
		if errors.Is(err, session.ErrNotReady) || errors.Is(err, session.ErrTerminal) {
			return rejectMessage(ctx, tx, id, "stale_gesture")
		}
		if errors.Is(err, session.ErrDenied) {
			return rejectMessage(ctx, tx, id, "permission_denied")
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,operation='native_stop',control_id=$3 WHERE id=$1`, id, human.UserID, control)
		return err
	})
}

// Releasing these exact recorded holds is part of admitting a new authorized
// Slack instruction. Recovery acknowledgement is scoped to this conversation
// and a gesture after the loss; unrelated holds remain untouched.
func resumeNativeStops(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, id uuid.UUID, p *messageAdmission) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('slack-stop:'||$1::uuid::text,0))`, p.thread); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT h.id FROM slack_requests stopped JOIN session_controls c ON (c.environment_id,c.session_id,c.id)=(stopped.environment_id,stopped.session_id,stopped.control_id) JOIN session_holds h ON (h.environment_id,h.id)=(c.environment_id,c.hold_id) JOIN slack_requests incoming ON incoming.id=$2
 WHERE stopped.thread_id=$1 AND stopped.status='accepted' AND stopped.operation='native_stop' AND h.released_at IS NULL AND stopped.source_occurred_at<incoming.source_occurred_at
 UNION SELECT h.id FROM session_holds h JOIN slack_requests incoming ON incoming.id=$2
 WHERE h.environment_id=$3 AND h.session_id=$4 AND h.kind='recovery_gap' AND h.released_at IS NULL AND h.created_at<incoming.source_occurred_at
 ORDER BY id`, p.thread, id, p.route.Target.EnvironmentID, p.session)
	if err != nil {
		return err
	}
	holds, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, hold := range holds {
		_, err = session.ControlSession(ctx, auditPolicy, tx, slackCaller(p.human, p.thread), session.SessionControlRequest{EnvironmentID: p.route.Target.EnvironmentID, SessionID: p.session, Kind: "resume", HoldID: hold, IdempotencyKey: "slack:" + id.String() + ":" + hold.String()})
		if err != nil {
			return err
		}
	}
	return nil
}
