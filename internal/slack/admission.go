package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

var errAdmissionExpired = errors.New("slack gesture expired during admission")

// messageAdmission is a disposable preflight snapshot, never an authorization
// grant. The receipt, connection and human authority are rechecked at commit.
type messageAdmission struct {
	human                   auth.Principal
	installation            uuid.UUID
	route                   slackroute.StartRoute
	message                 messageGesture
	root, actor             string
	occurred                time.Time
	user                    uuid.UUID
	thread, session, source uuid.UUID
}

func rejectMessage(ctx context.Context, tx pgx.Tx, id uuid.UUID, code string) error {
	_, err := tx.Exec(ctx, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error=$2 WHERE id=$1 AND status='received'`, id, code)
	return err
}

func readMessageAdmission(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*messageAdmission, error) {
	p := &messageAdmission{}
	var raw []byte
	var status, actor string
	var occurred time.Time
	var unexpired bool
	err := tx.QueryRow(ctx, `SELECT installation_id,payload,status,slack_user_id,source_occurred_at,expires_at>clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&p.installation, &raw, &status, &actor, &occurred, &unexpired)
	if err != nil || status != "received" {
		return nil, err
	}
	if json.Unmarshal(raw, &p.message) != nil || !validConversationID(p.message.Channel) {
		return nil, errGestureInvalid
	}
	p.actor = actor
	p.occurred = occurred
	p.root = p.message.Thread
	if p.root == "" {
		p.root = p.message.Timestamp
	}
	if !timestampPattern.MatchString(p.root) {
		return nil, errGestureInvalid
	}
	reject := func(code string) (*messageAdmission, error) { return nil, rejectMessage(ctx, tx, id, code) }
	if !unexpired {
		return reject("gesture_expired")
	}
	var env, target uuid.UUID
	err = tx.QueryRow(ctx, `SELECT environment_id,agent_id FROM slack_connections WHERE slack_installation_id=$1`, p.installation).Scan(&env, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return reject("binding_unavailable")
	}
	if err != nil {
		return nil, err
	}
	p.route.Target, err = slackroute.ReadConnection(ctx, tx, env, target, p.message.Channel)
	if errors.Is(err, slackroute.ErrTargetNotPublished) {
		return reject("binding_unavailable")
	}
	if err != nil {
		return nil, err
	}
	if p.route.Target.InstallationID != p.installation {
		return reject("thread_generation_unavailable")
	}
	var enabled, connected time.Time
	if err = tx.QueryRow(ctx, `SELECT p.created_at,i.connected_at FROM slack_connections p JOIN slack_installations i ON i.id=p.slack_installation_id WHERE p.id=$1`, p.route.Target.ConnectionID).Scan(&enabled, &connected); err != nil {
		return nil, err
	}
	if occurred.Before(enabled) || occurred.Before(connected) || occurred.Before(p.route.Target.AuthorizedAt) {
		return reject("stale_gesture")
	}
	var code string
	p.human, code, err = linkedHuman(ctx, tx, p.route.Target.TeamID, actor, p.installation, env, target, occurred)
	p.user = p.human.UserID
	if err != nil {
		return nil, err
	}
	if code != "" {
		return reject(code)
	}
	var owner uuid.UUID
	var deleted bool
	var peer, peerTeam *string
	err = tx.QueryRow(ctx, `SELECT t.id,t.front_session_id,s.id,c.connection_id,t.deleted_at IS NOT NULL,t.recipient_user_id,t.recipient_team_id
 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_thread_sources s ON s.thread_id=t.id AND s.session_id=t.front_session_id
 WHERE t.org_id=$1 AND t.team_id=$2 AND t.slack_channel_id=$3 AND t.thread_ts=$4`, p.route.Target.OrganizationID, p.route.Target.TeamID, p.message.Channel, p.root).Scan(&p.thread, &p.session, &p.source, &owner, &deleted, &peer, &peerTeam)
	if err == nil {
		if owner != p.route.Target.ConnectionID || deleted {
			return reject("thread_generation_unavailable")
		}
		if slackroute.IsDirectMessage(p.message.Channel) && (peer == nil || peerTeam == nil || *peer != actor || *peerTeam != p.route.Target.TeamID) {
			return reject("permission_denied")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if p.thread == uuid.Nil() {
		if slackroute.IsDirectMessage(p.message.Channel) && p.message.Thread != "" {
			// Out-of-order replies wait for the accepted root, bounded by receipt expiry.
			// They never turn an unlinked/rejected historical root into new work.
			return nil, nil
		}
		if threadInputCommand(p.message.Text, p.route.Target.BotUserID) != "" {
			return reject("command_unbound")
		}
		err = tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, env).Scan(&p.route.DeploymentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return reject("agent_unavailable")
		}
		if err != nil {
			return nil, err
		}
		p.route.HumanThreadTS, p.route.HumanActor = p.root, actor
	}
	return p, nil
}

// Admission never holds a SQL transaction across channel or root verification.
func admitMessage(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, trust computer.TrustIssuer, client *WebClient, id uuid.UUID) error {
	var p *messageAdmission
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error { var err error; p, err = readMessageAdmission(ctx, tx, id); return err })
	if err != nil || p == nil {
		return err
	}
	if client == nil {
		return ErrChannelVerification
	}
	name, err := client.verifyConversation(ctx, p.installation, p.route.Target.CredentialRevision, p.route.Target.TeamID, p.message.Channel, p.actor)
	if err != nil {
		var failure *channelVerificationFailure
		typed := errors.As(err, &failure)
		if typed && failure.result.Disposition == Rejected || !typed && errors.Is(err, ErrChannelVerification) {
			return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return rejectMessage(ctx, tx, id, "binding_unavailable") })
		}
		return err
	}
	if err = rememberChannelName(ctx, pool, p.installation, p.message.Channel, name); err != nil {
		return err
	}
	if p.thread == uuid.Nil() && p.message.Thread != "" {
		unrelated, err := client.resolveMentionRoot(ctx, pool, p.route.Target, p.root)
		if err != nil {
			return err
		}
		if !unrelated {
			return nil
		}
	}
	command := threadInputCommand(p.message.Text, p.route.Target.BotUserID)
	var input json.RawMessage
	if command == "" || p.message.UnsupportedFiles || len(p.message.FileIDs) > 0 {
		input, err = messageInput(p.message, p.route.Target.BotUserID)
	}
	if err != nil {
		return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
			code := "content_invalid"
			if errors.Is(err, content.ErrUnsupported) {
				code = "content_unsupported"
			}
			return rejectMessage(ctx, tx, id, code)
		})
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if command == "" || command == "wake" {
			if err := workergroup.LockExecutionSupply(ctx, tx, p.route.Target.EnvironmentID); err != nil {
				return err
			}
		}
		active, err := slackroute.LockConnections(ctx, tx, p.route.Target.EnvironmentID, []uuid.UUID{p.route.Target.ConnectionID})
		if err != nil {
			return err
		}
		if !active {
			return rejectMessage(ctx, tx, id, "binding_unavailable")
		}
		// Serialize competing mentions of one physical root across dedicated apps.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "slack-root:"+p.route.Target.OrganizationID.String()+":"+p.route.Target.TeamID+":"+p.message.Channel+":"+p.root); err != nil {
			return err
		}
		var status string
		if err = tx.QueryRow(ctx, `SELECT status FROM slack_requests WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
			return err
		}
		if status != "received" {
			return nil
		}
		current, err := readMessageAdmission(ctx, tx, id)
		if err != nil || current == nil {
			return err
		}
		if current.human.SlackLinkID != p.human.SlackLinkID || current.user != p.user || current.route.Target.ConnectionID != p.route.Target.ConnectionID || !current.route.Target.AuthorizedAt.Equal(p.route.Target.AuthorizedAt) {
			return slackroute.ErrConversationChanged
		}
		// A concurrent mention may have created the same front during remote checks.
		p.thread, p.session, p.source = current.thread, current.session, current.source
		if lifecycleAction(command) {
			return admitLifecycleMessage(ctx, auditPolicy, tx, id, p, command)
		}
		if handled, err := applyThreadInput(ctx, tx, id, p, command); err != nil || handled {
			return err
		}
		var receipt session.SendReceipt
		err = db.RunTx(ctx, tx, func(core pgx.Tx) error {
			var err error
			if p.thread == uuid.Nil() {
				var plan session.StartPlan
				// Public Slack destinations remain channel-only. The authenticated receipt
				// and internal route own a DM destination, not the public StartRequest.
				destination := &p.message.Channel
				if slackroute.IsDirectMessage(p.message.Channel) {
					destination = nil
				}
				caller := slackCaller(p.human, uuid.Nil())
				if destination != nil {
					caller.SlackConnectionID = p.route.Target.ConnectionID
				}
				plan, err = session.PlanStart(auditPolicy, caller, session.StartRequest{EnvironmentID: p.route.Target.EnvironmentID, Agent: p.route.Target.AgentName, SlackChannelID: destination, IdempotencyKey: "slack:" + id.String(), Input: input}, "create")
				if err == nil {
					err = session.LockStartCaller(ctx, core, plan)
				}
				if err == nil {
					// The connection was locked and found active at the start of
					// this transaction.
					receipt.Admission, err = admitRoutedRoot(ctx, core, trust, plan, p.route, true)
				}
				err = session.StartError(err)
			} else {
				if err = resumeNativeStops(ctx, auditPolicy, core, id, p); err != nil {
					return err
				}
				receipt, err = session.RouteSlackInput(ctx, auditPolicy, core, trust, slackCaller(p.human, p.thread), session.EnqueueRequest{EnvironmentID: p.route.Target.EnvironmentID, SessionID: p.session, IdempotencyKey: "slack:" + id.String(), Input: input})
			}
			if err != nil {
				return err
			}
			if p.thread != uuid.Nil() {
				var deleted bool
				if err = core.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM slack_threads WHERE id=$1 FOR SHARE`, p.thread).Scan(&deleted); err != nil {
					return err
				}
				if deleted {
					return session.ErrNotReady
				}
			}
			var unexpired bool
			if err = core.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&unexpired); err != nil {
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
				return rejectMessage(ctx, tx, id, "gesture_expired")
			case (errors.Is(err, session.ErrDenied) || errors.Is(err, computer.ErrDenied)):
				return rejectMessage(ctx, tx, id, "permission_denied")
			case errors.Is(err, computer.ErrExecutionSettling):
				return rejectMessage(ctx, tx, id, "execution_settling")
			case errors.Is(err, workergroup.ErrCapacityIncompatible):
				return rejectMessage(ctx, tx, id, "execution_capacity_incompatible")
			case errors.Is(err, workergroup.ErrCapacityExhausted):
				return rejectMessage(ctx, tx, id, "execution_capacity_exhausted")
			case errors.Is(err, computer.ErrExecutionLimit):
				return rejectMessage(ctx, tx, id, "execution_limit")
			case (errors.Is(err, session.ErrNotReady) || errors.Is(err, computer.ErrNotReady)), (errors.Is(err, session.ErrTerminal) || errors.Is(err, computer.ErrTerminal)):
				return rejectMessage(ctx, tx, id, "session_unavailable")
			case (errors.Is(err, session.ErrInvalidInput) || errors.Is(err, computer.ErrInvalidInput)), errors.Is(err, content.ErrUnsupported):
				return rejectMessage(ctx, tx, id, "content_unsupported")
			default:
				return err
			}
		}
		operation := "enqueue"
		if p.thread == uuid.Nil() {
			operation = "start"
			p.session = receipt.SessionID
			if err = tx.QueryRow(ctx, `SELECT t.id,s.id FROM slack_threads t JOIN slack_thread_sources s ON s.thread_id=t.id AND s.session_id=t.front_session_id WHERE t.environment_id=$1 AND t.front_session_id=$2`, p.route.Target.EnvironmentID, p.session).Scan(&p.thread, &p.source); err != nil {
				return err
			}
		}
		var message any
		if receipt.MessageID != uuid.Nil() {
			operation = "slack_route"
			message = receipt.MessageID
		}
		_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$8,turn_id=$7,message_id=$9 WHERE id=$1`, id, p.user, p.thread, p.source, p.route.Target.EnvironmentID, p.session, receipt.TurnID, operation, message)
		return err
	})
}

// applyThreadInput runs under the same root admission lock as conversational
// input. Controls do not enter the work/resume path. The source-time cutoff
// prevents delayed muted messages from becoming work after an unmute.
func applyThreadInput(ctx context.Context, tx pgx.Tx, id uuid.UUID, p *messageAdmission, command string) (bool, error) {
	if command == "aside" {
		return true, rejectMessage(ctx, tx, id, "input_ignored")
	}
	if p.thread == uuid.Nil() {
		return false, nil
	}
	var muted, accepting bool
	var changed, unmuted *time.Time
	if err := tx.QueryRow(ctx, `SELECT t.input_muted,t.input_changed_at,t.input_unmuted_at,s.status='open' FROM slack_threads t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) WHERE t.id=$1`, p.thread).Scan(&muted, &changed, &unmuted, &accepting); err != nil {
		return true, err
	}
	if command == "" {
		unmentioned := stripRoutingMention(p.message.Text, p.route.Target.BotUserID) == p.message.Text
		bareMention := strings.TrimSpace(stripRoutingMention(p.message.Text, p.route.Target.BotUserID)) == ""
		if accepting && (unmentioned || bareMention) && (muted || unmuted != nil && !p.occurred.After(*unmuted)) {
			return true, rejectMessage(ctx, tx, id, "input_ignored")
		}
		return false, nil
	}
	var open, unexpired bool
	if err := tx.QueryRow(ctx, `SELECT s.status='open',r.expires_at>clock_timestamp() FROM sessions s JOIN slack_requests r ON r.id=$3 WHERE s.environment_id=$1 AND s.id=$2 FOR SHARE OF s`, p.route.Target.EnvironmentID, p.session, id).Scan(&open, &unexpired); err != nil {
		return true, err
	}
	if !open {
		return true, rejectMessage(ctx, tx, id, "session_unavailable")
	}
	if !unexpired {
		return true, rejectMessage(ctx, tx, id, "gesture_expired")
	}
	if err := session.AuthorizeSessionOperation(ctx, tx, slackCaller(p.human, p.thread), p.route.Target.EnvironmentID, p.session, auth.PermissionSessionsInterrupt); err != nil {
		if errors.Is(err, session.ErrDenied) {
			return true, rejectMessage(ctx, tx, id, "permission_denied")
		}
		return true, err
	}
	if changed != nil && !p.occurred.After(*changed) {
		return true, rejectMessage(ctx, tx, id, "stale_gesture")
	}
	if _, err := tx.Exec(ctx, `UPDATE slack_threads SET input_muted=$2,input_changed_at=$3,input_unmuted_at=CASE WHEN input_muted AND NOT $2 THEN $3 ELSE input_unmuted_at END WHERE id=$1`, p.thread, command == "mute", p.occurred); err != nil {
		return true, err
	}
	_, err := tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$7 WHERE id=$1`, id, p.user, p.thread, p.source, p.route.Target.EnvironmentID, p.session, command)
	return true, err
}
