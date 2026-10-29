package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

func validateSlackStart(ctx context.Context, tx pgx.Tx, env, agent, deployment uuid.UUID, caller session.Caller, p *slackroute.StartRoute) error {
	if p == nil {
		return nil
	}
	if p.Target.EnvironmentID != env || p.Target.AgentID != agent || p.DeploymentID != deployment || !validConversationID(p.Target.ChannelID) {
		return slackroute.ErrConversationChanged
	}
	if slackroute.IsDirectMessage(p.Target.ChannelID) && (caller.Kind != "user" || p.HumanActor == "" || p.HumanThreadTS == "") {
		return session.ErrDenied
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
	if p.HumanThreadTS == "" && (len(p.Opening) == 0 || !json.Valid(p.Opening)) {
		return session.ErrInvalidInput
	}
	return nil
}

func createSlackRoot(ctx context.Context, tx pgx.Tx, env, session, turn, channel, sourceLink uuid.UUID, p slackroute.StartRoute) error {
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
	_, err := tx.Exec(ctx, `INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key,recipient_team_id,recipient_user_id,initial_recipient_link_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, thread, env, session, channel, p.Target.OrganizationID, p.Target.TeamID, p.Target.ChannelID, root, opening, recipientTeam, recipientUser, optionalLinkID(sourceLink))
	if err != nil {
		return err
	}
	if err = dbnotify.Notify(ctx, tx, "slack_delivery"); err != nil {
		return err
	}
	source, err := slackroute.AddMember(ctx, tx, env, thread, session)
	if err != nil {
		return err
	}
	if p.HumanThreadTS != "" {
		return nil
	}
	digest := sha256.Sum256(p.Opening)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path,closed_at) VALUES($1,$2,$3,$4,$5,1,$6,'opening',$7,$8,$9,'post',clock_timestamp())`, uuid.NewV7(), env, session, source, thread, opening, nullableTurn(turn), []byte(p.Opening), digest[:])
	return err
}

// Start admits a Session as session.Start does, routing the new root through
// Slack only when the request names a channel. The opening post is prepared outside the transaction; the
// thread, its membership and the opening post commit with the Session.
func Start(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, trust computer.TrustIssuer, preflight slackroute.StartPreflight, caller session.Caller, req session.StartRequest) (session.Admission, error) {
	if req.SlackChannelID == nil {
		return session.Start(ctx, auditPolicy, pool, trust, caller, req)
	}
	plan, err := session.PlanStart(auditPolicy, caller, req, "create")
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
		channel := *req.SlackChannelID
		var deployment uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, req.EnvironmentID).Scan(&deployment); err != nil {
			return session.ErrNotReady
		}
		target, err := slackroute.ReadConnection(ctx, tx, req.EnvironmentID, agentID, channel)
		if err != nil {
			return err
		}
		route = &slackroute.StartRoute{Target: target, DeploymentID: deployment}
		return nil
	})
	if err != nil {
		return session.Admission{}, session.StartError(err)
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
	if err = createSlackRoot(ctx, tx, plan.Request.EnvironmentID, admission.SessionID, admission.TurnID, channel, plan.Caller.Human.SlackLinkID, route); err != nil {
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
	if !definition.ValidSlackChannelID(channel) {
		return slackroute.ErrChannelUnavailable
	}
	connection, err := slackroute.ReadConnection(ctx, tx, env, agentID, channel)
	if err != nil {
		return err
	}
	if connection.ConnectionID != connectionID || at.Before(connection.AuthorizedAt) {
		return slackroute.ErrChannelUnavailable
	}
	opening, _ := json.Marshal(map[string]any{"text": "Started " + connection.AgentName + ".", "mrkdwn": false, "parse": "none", "link_names": false, "unfurl_links": false, "unfurl_media": false})
	return createSlackRoot(ctx, tx, env, sessionID, turn, binding, uuid.Nil(), slackroute.StartRoute{Target: connection, DeploymentID: deployment, Opening: opening})
}

func nullableTurn(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}
