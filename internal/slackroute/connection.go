package slackroute

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Connection is an internal preflight snapshot. Authored requests contain
// only the actual Slack channel ID; none of these resolved identities enter the
// caller's idempotency fingerprint.
type Connection struct {
	ConnectionID, RegistrationID, InstallationID, EnvironmentID, OrganizationID uuid.UUID
	AgentID                                                                     uuid.UUID
	TeamID, ChannelID, AppID, BotUserID, AgentName                              string
	CredentialRevision                                                          int64
	AuthorizedAt                                                                time.Time
}

// StartRoute is a preflighted Slack destination for a new root Session.
type StartRoute struct {
	Target                    Connection
	DeploymentID              uuid.UUID
	Opening                   json.RawMessage
	HumanThreadTS, HumanActor string
}

type StartContext struct {
	Route StartRoute
	Input json.RawMessage
}

// StartPreflight renders and checks the opening post outside the admission
// transaction.
type StartPreflight interface {
	PrepareStart(context.Context, StartContext) (json.RawMessage, error)
}

// ReadConnection returns the Agent's eligible connection for a channel.
func ReadConnection(ctx context.Context, tx pgx.Tx, env, agent uuid.UUID, channel string) (Connection, error) {
	var c Connection
	c.EnvironmentID = env
	c.ChannelID = channel
	c.AgentID = agent
	err := tx.QueryRow(ctx, `SELECT p.id,r.id,i.id,e.org_id,i.team_id,i.app_id,i.bot_user_id,a.key,i.credential_revision,i.authorized_at
 FROM slack_connections p JOIN environments e ON e.id=p.environment_id JOIN agents a ON (a.environment_id,a.id)=(p.environment_id,p.agent_id)
 JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.agent_id=$2 AND p.revoked_at IS NULL AND r.retired_at IS NULL AND r.credential_ciphertext IS NOT NULL
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.credential_ciphertext IS NOT NULL AND i.granted_scopes @> $3::text[]
 AND i.org_id=e.org_id AND r.org_id=e.org_id AND r.app_id=i.app_id`, env, agent, conversationScopes(channel)).Scan(&c.ConnectionID, &c.RegistrationID, &c.InstallationID, &c.OrganizationID, &c.TeamID, &c.AppID, &c.BotUserID, &c.AgentName, &c.CredentialRevision, &c.AuthorizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrTargetNotPublished
	}
	return c, err
}

// ResolveChannelBinding returns the channel binding for the connection and
// channel, creating it on first use.
func ResolveChannelBinding(ctx context.Context, tx pgx.Tx, c Connection) (uuid.UUID, error) {
	id := uuid.NewV7()
	tag, err := tx.Exec(ctx, `INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(connection_id,slack_channel_id) DO NOTHING`, id, c.EnvironmentID, c.ConnectionID, c.InstallationID, c.OrganizationID, c.TeamID, c.ChannelID)
	if err != nil {
		return uuid.Nil(), err
	}
	if tag.RowsAffected() == 1 {
		return id, nil
	}
	err = tx.QueryRow(ctx, `SELECT id FROM slack_channel_bindings WHERE connection_id=$1 AND environment_id=$2 AND installation_id=$3 AND slack_channel_id=$4`, c.ConnectionID, c.EnvironmentID, c.InstallationID, c.ChannelID).Scan(&id)
	return id, err
}

// CreateConnection records a new connection for the Agent's app registration.
func CreateConnection(ctx context.Context, tx pgx.Tx, id, org, env, agent, registration, user uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO slack_connections(id,org_id,environment_id,agent_id,slack_app_registration_id,created_by_user_id) VALUES($1,$2,$3,$4,$5,$6)`, id, org, env, agent, registration, user)
	return err
}

// AttachInstallation binds the connection to its first installation.
func AttachInstallation(ctx context.Context, tx pgx.Tx, connection, installation uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE slack_connections SET slack_installation_id=$2 WHERE id=$1 AND slack_installation_id IS NULL`, connection, installation)
	return err
}

// RevokeConnection ends the connection; revocation is idempotent.
func RevokeConnection(ctx context.Context, tx pgx.Tx, connection uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE slack_connections SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE id=$1`, connection)
	return err
}
