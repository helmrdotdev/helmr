package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

func ValidSlackChannelID(value string) bool { return definition.ValidSlackChannelID(value) }

// SlackConnection is an internal preflight snapshot. Authored requests contain
// only the actual Slack channel ID; none of these resolved identities enter the
// caller's idempotency fingerprint.
type SlackConnection struct {
	PublicationID, RegistrationID, InstallationID, EnvironmentID, OrganizationID uuid.UUID
	AgentID                                                                      uuid.UUID
	TeamID, ChannelID, AppID, BotUserID, AgentName                               string
	CredentialRevision                                                           int64
	AuthorizedAt                                                                 time.Time
}
type SlackStartSource struct {
	Connection                   SlackConnection
	ThreadID, SessionID, RouteID uuid.UUID
	ThreadTS                     string
}
type SlackStartRoute struct {
	Target                    SlackConnection
	DeploymentID              uuid.UUID
	Source                    *SlackStartSource
	Opening                   json.RawMessage
	HumanThreadTS, HumanActor string
}
type SlackStartContext struct {
	Route SlackStartRoute
	Input json.RawMessage
}
type SlackStartPreflight interface {
	PrepareStart(context.Context, SlackStartContext) (json.RawMessage, error)
}

func ReadSlackConnection(ctx context.Context, tx pgx.Tx, env, agent uuid.UUID, channel string) (SlackConnection, error) {
	var c SlackConnection
	c.EnvironmentID = env
	c.ChannelID = channel
	c.AgentID = agent
	err := tx.QueryRow(ctx, `SELECT p.id,r.id,i.id,e.org_id,i.team_id,i.app_id,i.bot_user_id,a.name,i.credential_revision,i.authorized_at
 FROM agent_publications p JOIN environments e ON e.id=p.environment_id JOIN agents a ON (a.environment_id,a.id)=(p.environment_id,p.agent_id)
 JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.agent_id=$2 AND p.revoked_at IS NULL AND r.retired_at IS NULL AND r.credential_ciphertext IS NOT NULL
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.credential_ciphertext IS NOT NULL AND i.granted_scopes @> $3::text[]
 AND i.organization_id=e.org_id AND r.organization_id=e.org_id AND r.app_id=i.app_id`, env, agent, RequiredSlackScopes()).Scan(&c.PublicationID, &c.RegistrationID, &c.InstallationID, &c.OrganizationID, &c.TeamID, &c.AppID, &c.BotUserID, &c.AgentName, &c.CredentialRevision, &c.AuthorizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrTargetNotPublished
	}
	return c, err
}

func readSlackStartSource(ctx context.Context, tx pgx.Tx, env, caller uuid.UUID) (*SlackStartSource, error) {
	var route *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT slack_channel_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, caller).Scan(&route); err != nil {
		return nil, err
	}
	if route == nil {
		return nil, nil
	}
	source := &SlackStartSource{SessionID: caller, RouteID: *route}
	c := &source.Connection
	c.EnvironmentID = env
	var root *string
	var active, abandoned bool
	err := tx.QueryRow(ctx, `SELECT t.id,t.thread_ts,c.publication_id,r.id,i.id,c.organization_id,c.team_id,c.slack_channel_id,i.app_id,i.bot_user_id,s.agent_id,a.name,i.credential_revision,i.authorized_at,
 p.revoked_at IS NULL AND r.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND t.deleted_at IS NULL,
 EXISTS(SELECT 1 FROM slack_posts post WHERE post.thread_id=t.id AND post.role='opening' AND (post.delivery_disposed_at IS NOT NULL OR (t.thread_ts IS NULL AND post.status IN ('failed','suppressed'))))
 FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id JOIN agent_publications p ON p.id=c.publication_id
 JOIN slack_installations i ON i.id=c.installation_id JOIN slack_app_registrations r ON r.id=i.app_registration_id
 JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
 WHERE t.environment_id=$1 AND t.front_session_id=$2 AND t.channel_id=$3`, env, caller, *route).Scan(&source.ThreadID, &root, &c.PublicationID, &c.RegistrationID, &c.InstallationID, &c.OrganizationID, &c.TeamID, &c.ChannelID, &c.AppID, &c.BotUserID, &c.AgentID, &c.AgentName, &c.CredentialRevision, &c.AuthorizedAt, &active, &abandoned)
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

// prepareStart returns an authorized exact receipt before live promotion or
// connection checks. All remote work happens after this transaction ends.
func prepareStart(ctx context.Context, pool db.TxBeginner, caller Caller, req StartRequest, method string, digest [32]byte) (*Admission, *SlackStartRoute, error) {
	if req.PreparedSlack != nil {
		return nil, req.PreparedSlack, nil
	}
	var receipt *Admission
	var route *SlackStartRoute
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if caller.Kind == "session" {
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
			if err := requireRootCaller(ctx, tx, caller); err != nil {
				return err
			}
		} else if err := authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		var agent, deployment uuid.UUID
		var err error
		if caller.Kind == "session" && method == "spawn" {
			err = tx.QueryRow(ctx, `SELECT d.agent_id,s.deployment_id FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.deployment_id)=(s.environment_id,s.deployment_id) WHERE s.environment_id=$1 AND s.id=$2 AND d.definition_key=$3`, req.EnvironmentID, caller.ID, req.Agent).Scan(&agent, &deployment)
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND name=$2`, req.EnvironmentID, req.Agent).Scan(&agent)
		}
		if err != nil {
			return err
		}
		if req.RetryKey != "" {
			var prior []byte
			var value Admission
			err = tx.QueryRow(ctx, `SELECT session_id,id,seq,seq=1,request_digest FROM turns WHERE environment_id=$1 AND caller_kind=$2 AND caller_id=$3 AND admission_method=$4 AND target_id=$5 AND retry_key=$6`, req.EnvironmentID, caller.Kind, caller.ID, method, agent, req.RetryKey).Scan(&value.SessionID, &value.TurnID, &value.Sequence, &value.Created, &prior)
			if err == nil {
				if caller.Kind == "session" {
					owner, e := lockSession(ctx, tx, req.EnvironmentID, caller.ID)
					if e != nil {
						return e
					}
					if e = executionReceipt(ctx, tx, caller.Execution, owner); e != nil {
						return e
					}
				} else if !value.Created {
					if e := authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owners{}); e != nil {
						return e
					}
				}
				if !bytes.Equal(prior, digest[:]) {
					return ErrConflict
				}
				receipt = &value
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if caller.Kind == "session" {
			owner, e := lockSession(ctx, tx, req.EnvironmentID, caller.ID)
			if e != nil {
				return e
			}
			if e = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owner); e != nil {
				return e
			}
		}
		if method == "spawn" {
			return nil
		}
		// Resolve keyed continuation before considering current target defaults.
		if req.SessionKey != nil {
			var pinned *uuid.UUID
			err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND session_key=$3`, req.EnvironmentID, agent, *req.SessionKey).Scan(&pinned)
			if err == nil {
				if req.SlackChannelID != nil {
					if pinned == nil {
						return ErrSlackDestinationConflict
					}
					var channel string
					if err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM slack_channels WHERE id=$1`, *pinned).Scan(&channel); err != nil {
						return err
					}
					if channel != *req.SlackChannelID {
						return ErrSlackDestinationConflict
					}
				}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var source *SlackStartSource
		channel := ""
		if caller.Kind == "session" {
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
		if err = tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, req.EnvironmentID).Scan(&deployment); err != nil {
			return ErrNotReady
		}
		target, err := ReadSlackConnection(ctx, tx, req.EnvironmentID, agent, channel)
		if err != nil {
			return err
		}
		if source != nil && (source.Connection.TeamID != target.TeamID || source.Connection.OrganizationID != target.OrganizationID) {
			return ErrTargetWorkspaceMismatch
		}
		route = &SlackStartRoute{Target: target, DeploymentID: deployment, Source: source}
		return nil
	})
	if err != nil {
		return nil, nil, hideMissing(err)
	}
	if receipt != nil || route == nil {
		return receipt, route, nil
	}
	if req.SlackPreflight == nil {
		return nil, nil, ErrSlackChannelUnavailable
	}
	route.Opening, err = req.SlackPreflight.PrepareStart(ctx, SlackStartContext{Route: *route, Input: req.Input})
	if err != nil {
		return nil, nil, err
	}
	return nil, route, nil
}

func validateSlackStart(ctx context.Context, tx pgx.Tx, env, agent, deployment uuid.UUID, caller Caller, p *SlackStartRoute) error {
	if p == nil {
		return nil
	}
	if p.Target.EnvironmentID != env || p.Target.AgentID != agent || p.DeploymentID != deployment || !ValidSlackChannelID(p.Target.ChannelID) {
		return ErrConversationChanged
	}
	c, err := ReadSlackConnection(ctx, tx, env, agent, p.Target.ChannelID)
	if err != nil {
		return ErrConversationChanged
	}
	if c.PublicationID != p.Target.PublicationID || c.RegistrationID != p.Target.RegistrationID || c.InstallationID != p.Target.InstallationID || c.TeamID != p.Target.TeamID || c.OrganizationID != p.Target.OrganizationID || !c.AuthorizedAt.Equal(p.Target.AuthorizedAt) {
		return ErrConversationChanged
	}
	if caller.Kind == "session" {
		if p.HumanThreadTS != "" {
			return ErrDenied
		}
		source, err := readSlackStartSource(ctx, tx, env, caller.ID)
		if err != nil {
			return err
		}
		if (source == nil) != (p.Source == nil) || source != nil && (source.ThreadID != p.Source.ThreadID || source.RouteID != p.Source.RouteID || source.ThreadTS != p.Source.ThreadTS || !source.Connection.AuthorizedAt.Equal(p.Source.Connection.AuthorizedAt)) {
			return ErrConversationChanged
		}
	}
	if p.Source != nil && (p.Source.Connection.TeamID != c.TeamID || p.Source.Connection.ChannelID != c.ChannelID || p.Source.Connection.OrganizationID != c.OrganizationID) {
		return ErrConversationChanged
	}
	if p.HumanThreadTS == "" && (len(p.Opening) == 0 || !json.Valid(p.Opening)) {
		return ErrInvalidInput
	}
	return nil
}

func ResolveSlackRoute(ctx context.Context, tx pgx.Tx, c SlackConnection) (uuid.UUID, error) {
	id := uuid.NewV7()
	tag, err := tx.Exec(ctx, `INSERT INTO slack_channels(id,environment_id,publication_id,installation_id,organization_id,team_id,slack_channel_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(publication_id,slack_channel_id) DO NOTHING`, id, c.EnvironmentID, c.PublicationID, c.InstallationID, c.OrganizationID, c.TeamID, c.ChannelID)
	if err != nil {
		return uuid.Nil(), err
	}
	if tag.RowsAffected() == 1 {
		return id, nil
	}
	err = tx.QueryRow(ctx, `SELECT id FROM slack_channels WHERE publication_id=$1 AND environment_id=$2 AND installation_id=$3 AND slack_channel_id=$4`, c.PublicationID, c.EnvironmentID, c.InstallationID, c.ChannelID).Scan(&id)
	return id, err
}

func createSlackRoot(ctx context.Context, tx pgx.Tx, env, session, turn, channel uuid.UUID, p SlackStartRoute) error {
	thread, source := uuid.NewV7(), uuid.NewV7()
	var opening, root, recipientTeam, recipientUser any
	if p.HumanThreadTS != "" {
		root = p.HumanThreadTS
		if p.HumanActor != "" {
			recipientTeam = p.Target.TeamID
			recipientUser = p.HumanActor
		}
	} else {
		opening = "opening:" + turn.String()
	}
	_, err := tx.Exec(ctx, `INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key,recipient_team_id,recipient_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, thread, env, session, channel, p.Target.OrganizationID, p.Target.TeamID, p.Target.ChannelID, root, opening, recipientTeam, recipientUser)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($1,$2,$3,$4)`, source, thread, env, session); err != nil {
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
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,source_thread_id,seq,publication_key,role,turn_id,payload,payload_digest,presentation_path,closed_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,'opening',$8,$9,$10,'post',clock_timestamp())`, uuid.NewV7(), env, session, source, thread, sourceThread, opening, turn, []byte(p.Opening), digest[:])
	return err
}
