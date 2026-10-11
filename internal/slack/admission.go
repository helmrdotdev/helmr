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

var errAdmissionExpired = errors.New("slack gesture expired during admission")

// messageAdmission is a disposable preflight snapshot, never an authorization
// grant. The receipt, connection and human authority are rechecked at commit.
type messageAdmission struct {
	installation            uuid.UUID
	route                   agent.SlackStartRoute
	message                 messageGesture
	root                    string
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
	if json.Unmarshal(raw, &p.message) != nil || !agent.ValidSlackChannelID(p.message.Channel) {
		return nil, errGestureInvalid
	}
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
	err = tx.QueryRow(ctx, `SELECT environment_id,agent_id FROM agent_publications WHERE slack_installation_id=$1`, p.installation).Scan(&env, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return reject("binding_unavailable")
	}
	if err != nil {
		return nil, err
	}
	p.route.Target, err = agent.ReadSlackConnection(ctx, tx, env, target, p.message.Channel)
	if errors.Is(err, agent.ErrTargetNotPublished) {
		return reject("binding_unavailable")
	}
	if err != nil {
		return nil, err
	}
	if p.route.Target.InstallationID != p.installation {
		return reject("thread_generation_unavailable")
	}
	var enabled, connected time.Time
	if err = tx.QueryRow(ctx, `SELECT p.created_at,i.connected_at FROM agent_publications p JOIN slack_installations i ON i.id=p.slack_installation_id WHERE p.id=$1`, p.route.Target.PublicationID).Scan(&enabled, &connected); err != nil {
		return nil, err
	}
	if occurred.Before(enabled) || occurred.Before(connected) || occurred.Before(p.route.Target.AuthorizedAt) {
		return reject("stale_gesture")
	}
	var code string
	p.user, code, err = linkedHuman(ctx, tx, p.route.Target.TeamID, actor, env, occurred, false)
	if err != nil {
		return nil, err
	}
	if code != "" {
		return reject(code)
	}
	var owner uuid.UUID
	var deleted bool
	err = tx.QueryRow(ctx, `SELECT t.id,t.front_session_id,s.id,c.publication_id,t.deleted_at IS NOT NULL
 FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id JOIN slack_thread_sources s ON s.thread_id=t.id AND s.session_id=t.front_session_id
 WHERE t.organization_id=$1 AND t.team_id=$2 AND t.slack_channel_id=$3 AND t.thread_ts=$4`, p.route.Target.OrganizationID, p.route.Target.TeamID, p.message.Channel, p.root).Scan(&p.thread, &p.session, &p.source, &owner, &deleted)
	if err == nil {
		if owner != p.route.Target.PublicationID || deleted {
			return reject("thread_generation_unavailable")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if p.thread == uuid.Nil() {
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
func admitMessage(ctx context.Context, pool db.TxBeginner, trust agent.ComputerTrustIssuer, client *WebClient, id uuid.UUID) error {
	var p *messageAdmission
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error { var err error; p, err = readMessageAdmission(ctx, tx, id); return err })
	if err != nil || p == nil {
		return err
	}
	if client == nil {
		return ErrChannelVerification
	}
	if _, err = client.verifyChannel(ctx, p.installation, p.route.Target.CredentialRevision, p.route.Target.TeamID, p.message.Channel); err != nil {
		var failure *channelVerificationFailure
		typed := errors.As(err, &failure)
		if typed && failure.result.Disposition == Rejected || !typed && errors.Is(err, ErrChannelVerification) {
			return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return rejectMessage(ctx, tx, id, "binding_unavailable") })
		}
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
	input, err := messageInput(p.message, p.route.Target.BotUserID)
	if err != nil {
		return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
			code := "content_invalid"
			if errors.Is(err, conversation.ErrUnsupported) {
				code = "content_unsupported"
			}
			return rejectMessage(ctx, tx, id, code)
		})
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		active, err := agent.LockSlackPublications(ctx, tx, p.route.Target.EnvironmentID, []uuid.UUID{p.route.Target.PublicationID})
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
		if current.user != p.user || current.route.Target.PublicationID != p.route.Target.PublicationID || !current.route.Target.AuthorizedAt.Equal(p.route.Target.AuthorizedAt) {
			return agent.ErrConversationChanged
		}
		// A concurrent mention may have created the same front during remote checks.
		p.thread, p.session, p.source = current.thread, current.session, current.source
		var receipt agent.SendReceipt
		err = db.RunTx(ctx, tx, func(core pgx.Tx) error {
			var err error
			if p.thread == uuid.Nil() {
				receipt.Admission, err = agent.Start(ctx, core, trust, agent.Caller{Kind: "user", ID: p.user}, agent.StartRequest{EnvironmentID: p.route.Target.EnvironmentID, Agent: p.route.Target.AgentName, SlackChannelID: &p.message.Channel, PreparedSlack: &p.route, RetryKey: "slack:" + id.String(), Input: input})
			} else {
				receipt, err = agent.Send(ctx, core, agent.Caller{Kind: "user", ID: p.user}, agent.EnqueueRequest{EnvironmentID: p.route.Target.EnvironmentID, SessionID: p.session, RetryKey: "slack:" + id.String(), Input: input})
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
					return agent.ErrNotReady
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
			case errors.Is(err, agent.ErrDenied):
				return rejectMessage(ctx, tx, id, "permission_denied")
			case errors.Is(err, agent.ErrNotReady), errors.Is(err, agent.ErrTerminal):
				return rejectMessage(ctx, tx, id, "session_unavailable")
			case errors.Is(err, agent.ErrInvalidInput), errors.Is(err, conversation.ErrUnsupported):
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
			operation = "send"
			message = receipt.MessageID
		}
		_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$8,turn_id=$7,message_id=$9 WHERE id=$1`, id, p.user, p.thread, p.source, p.route.Target.EnvironmentID, p.session, receipt.TurnID, operation, message)
		return err
	})
}
