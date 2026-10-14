package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

var (
	ErrSlackDestinationConflict      = errors.New("session Slack destination conflicts")
	ErrSourceConversationPending     = errors.New("source conversation is still being delivered; retry after its Slack thread is confirmed")
	ErrSourceConversationUnavailable = errors.New("source Slack conversation is unavailable")
	ErrTargetWorkspaceMismatch       = errors.New("target Agent is connected to a different Slack workspace")
	ErrStartMessageTooLarge          = errors.New("start input and attribution must fit in one Slack message")
)

func readSlackStartSource(ctx context.Context, tx pgx.Tx, env, caller uuid.UUID) (*slackroute.StartSource, error) {
	route, err := slackroute.ChannelOf(ctx, tx, env, caller)
	if err != nil || route == nil {
		return nil, err
	}
	source := &slackroute.StartSource{SessionID: caller, RouteID: *route}
	c := &source.Connection
	c.EnvironmentID = env
	var root *string
	var active, abandoned bool
	err = tx.QueryRow(ctx, `SELECT t.id,t.thread_ts,c.connection_id,r.id,i.id,c.org_id,c.team_id,c.slack_channel_id,i.app_id,i.bot_user_id,s.agent_id,a.key,i.credential_revision,i.authorized_at,
 p.revoked_at IS NULL AND r.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND t.deleted_at IS NULL,
 EXISTS(SELECT 1 FROM slack_posts post WHERE post.thread_id=t.id AND post.role='opening' AND (post.delivery_disposed_at IS NOT NULL OR (t.thread_ts IS NULL AND post.status IN ('failed','suppressed'))))
 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_connections p ON p.id=c.connection_id
 JOIN slack_installations i ON i.id=c.installation_id JOIN slack_app_registrations r ON r.id=i.app_registration_id
 JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
 WHERE t.environment_id=$1 AND t.front_session_id=$2 AND t.slack_channel_binding_id=$3`, env, caller, *route).Scan(&source.ThreadID, &root, &c.ConnectionID, &c.RegistrationID, &c.InstallationID, &c.OrganizationID, &c.TeamID, &c.ChannelID, &c.AppID, &c.BotUserID, &c.AgentID, &c.AgentName, &c.CredentialRevision, &c.AuthorizedAt, &active, &abandoned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSourceConversationUnavailable
	}
	if err != nil {
		return nil, err
	}
	if !active || abandoned {
		return nil, ErrSourceConversationUnavailable
	}
	if root == nil {
		return nil, ErrSourceConversationPending
	}
	source.ThreadTS = *root
	return source, nil
}

func validateSlackStart(ctx context.Context, tx pgx.Tx, env, agent, deployment uuid.UUID, caller session.Caller, p *slackroute.StartRoute) error {
	if p == nil {
		return nil
	}
	if p.Target.EnvironmentID != env || p.Target.AgentID != agent || p.DeploymentID != deployment || !definition.ValidSlackChannelID(p.Target.ChannelID) {
		return slackroute.ErrConversationChanged
	}
	var inputKind string
	if err := tx.QueryRow(ctx, `SELECT input_kind FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3`, env, agent, deployment).Scan(&inputKind); err != nil {
		return err
	}
	if inputKind != "text" {
		return session.ErrInvalidInput
	}
	c, err := slackroute.ReadConnection(ctx, tx, env, agent, p.Target.ChannelID)
	if err != nil {
		return slackroute.ErrConversationChanged
	}
	if c.ConnectionID != p.Target.ConnectionID || c.RegistrationID != p.Target.RegistrationID || c.InstallationID != p.Target.InstallationID || c.TeamID != p.Target.TeamID || c.OrganizationID != p.Target.OrganizationID || !c.AuthorizedAt.Equal(p.Target.AuthorizedAt) {
		return slackroute.ErrConversationChanged
	}
	if caller.Kind == "session" {
		if p.HumanThreadTS != "" {
			return session.ErrDenied
		}
		source, err := readSlackStartSource(ctx, tx, env, caller.ID)
		if err != nil {
			return err
		}
		if (source == nil) != (p.Source == nil) || source != nil && (source.ThreadID != p.Source.ThreadID || source.RouteID != p.Source.RouteID || source.ThreadTS != p.Source.ThreadTS || !source.Connection.AuthorizedAt.Equal(p.Source.Connection.AuthorizedAt)) {
			return slackroute.ErrConversationChanged
		}
	}
	if p.Source != nil && (p.Source.Connection.TeamID != c.TeamID || p.Source.Connection.ChannelID != c.ChannelID || p.Source.Connection.OrganizationID != c.OrganizationID) {
		return slackroute.ErrConversationChanged
	}
	if p.HumanThreadTS == "" && (len(p.Opening) == 0 || !json.Valid(p.Opening)) {
		return session.ErrInvalidInput
	}
	return nil
}

func createSlackRoot(ctx context.Context, tx pgx.Tx, env, session, turn, channel uuid.UUID, p slackroute.StartRoute) error {
	thread := uuid.NewV7()
	var opening, root, recipientTeam, recipientUser any
	if p.HumanThreadTS != "" {
		root = p.HumanThreadTS
		if p.HumanActor != "" {
			recipientTeam = p.Target.TeamID
			recipientUser = p.HumanActor
		}
	} else {
		opening = "opening:" + session.String()
	}
	_, err := tx.Exec(ctx, `INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key,recipient_team_id,recipient_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, thread, env, session, channel, p.Target.OrganizationID, p.Target.TeamID, p.Target.ChannelID, root, opening, recipientTeam, recipientUser)
	if err != nil {
		return err
	}
	source, err := slackroute.AddMember(ctx, tx, env, thread, session)
	if err != nil {
		return err
	}
	if p.HumanThreadTS != "" {
		return nil
	}
	var sourceThread any
	if p.Source != nil {
		sourceThread = p.Source.ThreadID
	}
	digest := sha256.Sum256(p.Opening)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,source_thread_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path,closed_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,'opening',$8,$9,$10,'post',clock_timestamp())`, uuid.NewV7(), env, session, source, thread, sourceThread, opening, nullableTurn(turn), []byte(p.Opening), digest[:])
	return err
}

// Start admits a Session as session.Start does, routing the new root through
// Slack when the request names a channel or the runtime caller's root has a
// Slack route. The opening post is prepared outside the transaction; the
// thread, its membership and the opening post commit with the Session.
func Start(ctx context.Context, pool db.TxBeginner, trust computer.TrustIssuer, preflight slackroute.StartPreflight, caller session.Caller, req session.StartRequest) (session.Admission, error) {
	if req.SlackChannelID == nil && caller.Kind != "session" {
		return session.Start(ctx, pool, trust, caller, req)
	}
	plan, err := session.PlanStart(caller, req, "create")
	if err != nil {
		return session.Admission{}, err
	}
	receipt, agentID, err := session.StartReceipt(ctx, pool, plan)
	if err != nil {
		return session.Admission{}, err
	}
	if receipt != nil {
		return *receipt, nil
	}
	var route *slackroute.StartRoute
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var source *slackroute.StartSource
		channel := ""
		if caller.Kind == "session" {
			var err error
			source, err = readSlackStartSource(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
			if source != nil {
				channel = source.Connection.ChannelID
			}
		}
		if req.SlackChannelID != nil {
			channel = *req.SlackChannelID
		}
		if channel == "" {
			return nil
		}
		var deployment uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, req.EnvironmentID).Scan(&deployment); err != nil {
			return session.ErrNotReady
		}
		target, err := slackroute.ReadConnection(ctx, tx, req.EnvironmentID, agentID, channel)
		if err != nil {
			return err
		}
		if source != nil && (source.Connection.TeamID != target.TeamID || source.Connection.OrganizationID != target.OrganizationID) {
			return ErrTargetWorkspaceMismatch
		}
		route = &slackroute.StartRoute{Target: target, DeploymentID: deployment, Source: source}
		return nil
	})
	if err != nil {
		return session.Admission{}, session.StartError(err)
	}
	if route == nil {
		return session.Start(ctx, pool, trust, caller, req)
	}
	if preflight == nil {
		return session.Admission{}, slackroute.ErrChannelUnavailable
	}
	route.Opening, err = preflight.PrepareStart(ctx, slackroute.StartContext{Route: *route, Input: plan.Request.Input})
	if err != nil {
		return session.Admission{}, err
	}
	var result session.Admission
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := session.LockStartCaller(ctx, tx, plan); err != nil {
			return err
		}
		connections := []uuid.UUID{route.Target.ConnectionID}
		if route.Source != nil {
			connections = append(connections, route.Source.Connection.ConnectionID)
		}
		active, err := slackroute.LockConnections(ctx, tx, req.EnvironmentID, connections)
		if err != nil {
			return err
		}
		result, err = admitRoutedRoot(ctx, tx, trust, plan, *route, active)
		return err
	})
	if err != nil {
		return session.Admission{}, session.StartError(err)
	}
	return result, nil
}

// admitRoutedRoot admits the planned Session and records its Slack root. The
// caller holds LockStartCaller and the route's connection locks; active is
// whether those connections still admit work. An exact retry returns its
// receipt before connection eligibility is considered.
func admitRoutedRoot(ctx context.Context, tx pgx.Tx, trust computer.TrustIssuer, plan session.StartPlan, route slackroute.StartRoute, active bool) (session.Admission, error) {
	// A binding insert can wait on promotion's first use of the same channel.
	// Take that wait at the Slack step, before admission locks the Environment.
	// Inactive routes still reach the retained receipt before fresh eligibility.
	var channel uuid.UUID
	if active {
		var err error
		channel, err = slackroute.ResolveChannelBinding(ctx, tx, route.Target)
		if err != nil {
			return session.Admission{}, err
		}
	}
	admission, pin, err := session.AdmitStart(ctx, tx, trust, plan)
	if err != nil || !pin.Fresh {
		return admission, err
	}
	if !active {
		return session.Admission{}, slackroute.ErrConversationChanged
	}
	if err = validateSlackStart(ctx, tx, plan.Request.EnvironmentID, pin.AgentID, pin.DeploymentID, plan.Caller, &route); err != nil {
		return session.Admission{}, err
	}
	if err = createSlackRoot(ctx, tx, plan.Request.EnvironmentID, admission.SessionID, admission.TurnID, channel, route); err != nil {
		return session.Admission{}, err
	}
	return admission, nil
}

// CreateScheduledRoot records the Slack thread and opening post of a Session
// a trigger started for a Slack destination. The caller holds the channel's
// route locks and admitted the Session in the same transaction.
func CreateScheduledRoot(ctx context.Context, tx pgx.Tx, env, sessionID, turn, binding, agentID, deployment uuid.UUID, at time.Time) error {
	var inputKind string
	if err := tx.QueryRow(ctx, `SELECT input_kind FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3`, env, agentID, deployment).Scan(&inputKind); err != nil {
		return err
	}
	if inputKind != "text" {
		return session.ErrInvalidInput
	}
	var channel string
	var connectionID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT connection_id,slack_channel_id FROM slack_channel_bindings WHERE environment_id=$1 AND id=$2`, env, binding).Scan(&connectionID, &channel); err != nil {
		return err
	}
	connection, err := slackroute.ReadConnection(ctx, tx, env, agentID, channel)
	if err != nil {
		return err
	}
	if connection.ConnectionID != connectionID || at.Before(connection.AuthorizedAt) {
		return slackroute.ErrChannelUnavailable
	}
	opening, _ := json.Marshal(map[string]any{"text": "Started " + connection.AgentName + ".", "mrkdwn": false, "parse": "none", "link_names": false, "unfurl_links": false, "unfurl_media": false})
	return createSlackRoot(ctx, tx, env, sessionID, turn, binding, slackroute.StartRoute{Target: connection, DeploymentID: deployment, Opening: opening})
}

func nullableTurn(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}
