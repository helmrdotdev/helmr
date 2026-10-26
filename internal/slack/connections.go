package slack

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrConnectionUnavailable = errors.New("the Agent's Slack connection is unavailable")

type ConnectionSetup struct {
	ID             uuid.UUID `json:"id"`
	RegistrationID uuid.UUID `json:"registration_id"`
}

// BeginConnection reserves the Agent's sole connection slot, including pending
// setup. A failed setup must be discarded before another can be created.
func BeginConnection(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID, mode Transport) (ConnectionSetup, error) {
	if !mode.enabled() {
		return ConnectionSetup{}, ErrConnectionUnavailable
	}
	setup := ConnectionSetup{ID: uuid.NewV7(), RegistrationID: uuid.NewV7()}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockAgentManager(ctx, tx, org, user, environment, agent, auth.PermissionAgentConnectionsCreate); err != nil {
			return err
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 FOR NO KEY UPDATE`, environment, org).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		if err != nil {
			return err
		}
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN environments e ON e.id=a.environment_id JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(a.environment_id,a.id,e.current_deployment_id) WHERE a.environment_id=$1 AND a.id=$2 AND a.deleted_at IS NULL AND d.input_kind='text') AND NOT EXISTS(SELECT 1 FROM slack_connections WHERE environment_id=$1 AND agent_id=$2 AND revoked_at IS NULL)`, environment, agent).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrConnectionUnavailable
		}
		_, err = tx.Exec(ctx, `INSERT INTO slack_app_registrations(id,org_id,created_by_user_id,transport) VALUES($1,$2,$3,$4)`, setup.RegistrationID, org, user, mode)
		if err != nil {
			return err
		}
		return slackroute.CreateConnection(ctx, tx, setup.ID, org, environment, agent, setup.RegistrationID, user)
	})
	if err != nil {
		return ConnectionSetup{}, err
	}
	return setup, nil
}

type SocketHealth struct {
	Status          string     `json:"status"`
	Reason          *string    `json:"reason"`
	LastConnectedAt *time.Time `json:"last_connected_at"`
	LastReceivedAt  *time.Time `json:"last_received_at"`
}

type ConnectionView struct {
	Socket                   *SocketHealth `json:"socket,omitempty"`
	Transport                Transport     `json:"transport"`
	ID                       uuid.UUID     `json:"id"`
	RegistrationID           uuid.UUID     `json:"registration_id"`
	InstallationID           *uuid.UUID    `json:"installation_id"`
	Status                   string        `json:"status"`
	ClientID                 *string       `json:"client_id"`
	DirectMessagesAuthorized bool          `json:"direct_messages_authorized"`
	CredentialsConfigured    bool          `json:"credentials_configured"`
	AppID                    *string       `json:"app_id"`
	TeamID                   *string       `json:"team_id"`
	WorkspaceName            *string       `json:"workspace_name"`
	AppName                  *string       `json:"app_name"`
	AppIconURL               *string       `json:"app_icon_url"`
}

func ConnectionAgent(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID, name string, permission auth.Permission) (uuid.UUID, error) {
	if auth.PermissionResourceType(permission) != "agent_connections" {
		return uuid.Nil(), errInstallationAuthority
	}
	var agent uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT a.id FROM agents a JOIN environments e ON e.id=a.environment_id WHERE a.environment_id=$1 AND a.key=$2 AND e.org_id=$3`, environment, name, org).Scan(&agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		if err != nil {
			return err
		}
		return lockAgentManager(ctx, tx, org, user, environment, agent, permission)
	})
	return agent, err
}

func GetConnection(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID, permission auth.Permission) (*ConnectionView, error) {
	if auth.PermissionResourceType(permission) != "agent_connections" {
		return nil, errInstallationAuthority
	}
	var view *ConnectionView
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockAgentManager(ctx, tx, org, user, environment, agent, permission); err != nil {
			return err
		}
		var value ConnectionView
		err := tx.QueryRow(ctx, `SELECT p.id,r.id,p.slack_installation_id,
 CASE WHEN i.id IS NULL THEN 'setup_incomplete' WHEN i.disconnected_at IS NOT NULL OR r.retired_at IS NOT NULL THEN 'disconnected' WHEN i.authorization_lost_at IS NOT NULL THEN 'reauthorization_required' ELSE 'connected' END,
 r.transport,r.client_id,r.credential_revision>0,r.app_id,i.team_id,i.workspace_name,r.app_name,r.app_icon_url,COALESCE(i.granted_scopes @> $4::text[],false)
 FROM slack_connections p JOIN environments e ON e.id=p.environment_id JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id LEFT JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.agent_id=$2 AND e.org_id=$3 AND r.org_id=$3 AND p.revoked_at IS NULL`, environment, agent, org, slackroute.DirectMessageScopes()).Scan(&value.ID, &value.RegistrationID, &value.InstallationID, &value.Status, &value.Transport, &value.ClientID, &value.CredentialsConfigured, &value.AppID, &value.TeamID, &value.WorkspaceName, &value.AppName, &value.AppIconURL, &value.DirectMessagesAuthorized)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if value.Transport == TransportSocket {
			health := SocketHealth{Status: "disconnected"}
			err := tx.QueryRow(ctx, `SELECT
 CASE WHEN sc.credential_revision<>r.credential_revision THEN 'reconnecting'
 WHEN sc.status IN ('connecting','connected') AND sc.expires_at<=clock_timestamp() THEN 'reconnecting'
 ELSE sc.status END,
 CASE WHEN sc.credential_revision=r.credential_revision THEN sc.reason ELSE NULL END,
 sc.last_connected_at,sc.last_received_at
 FROM slack_socket_connections sc JOIN slack_app_registrations r ON r.id=sc.app_registration_id
 WHERE sc.app_registration_id=$1 AND sc.installation_id=$2`, value.RegistrationID, value.InstallationID).Scan(&health.Status, &health.Reason, &health.LastConnectedAt, &health.LastReceivedAt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if value.Status != "connected" {
				health.Status = "disconnected"
				health.Reason = nil
			}
			value.Socket = &health
		}
		view = &value
		return nil
	})
	return view, err
}

// DisconnectConnection fences the exact generation, including pending setup.
// Running work remains available through Console/API and is not cancelled.
type ConnectionDisconnection struct {
	causeAgent                   *uuid.UUID
	registration                 uuid.UUID
	installation                 *uuid.UUID
	connection, environment, org uuid.UUID
	active                       bool
}

// LockConnectionDisconnection takes the Slack generation locks before the
// caller's Environment lock. Apply joins that caller's transaction.
func LockConnectionDisconnection(ctx context.Context, tx pgx.Tx, org, user, environment, connection uuid.UUID) (*ConnectionDisconnection, error) {
	if err := lockConnectionManager(ctx, tx, org, user, connection, auth.PermissionAgentConnectionsDelete); err != nil {
		return nil, err
	}
	return lockConnectionDisconnection(ctx, tx, org, environment, connection)
}

// LockAgentDeletionDisconnection limits deletion authority to the connection of
// the exact Agent being deleted. It does not grant connection administration.
func LockAgentDeletionDisconnection(ctx context.Context, tx pgx.Tx, org, user, environment, agent, connection uuid.UUID) (*ConnectionDisconnection, error) {
	if err := lockAgentManager(ctx, tx, org, user, environment, agent, auth.PermissionAgentsDelete); err != nil {
		return nil, err
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_connections WHERE id=$1 AND environment_id=$2 AND agent_id=$3)`, connection, environment, agent).Scan(&matches); err != nil {
		return nil, err
	}
	if !matches {
		return nil, ErrConnectionUnavailable
	}
	plan, err := lockConnectionDisconnection(ctx, tx, org, environment, connection)
	if err == nil {
		plan.causeAgent = &agent
	}
	return plan, err
}

func lockConnectionDisconnection(ctx context.Context, tx pgx.Tx, org, environment, connection uuid.UUID) (*ConnectionDisconnection, error) {
	var registration uuid.UUID
	var installation *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT p.slack_app_registration_id,p.slack_installation_id FROM slack_connections p JOIN environments e ON e.id=p.environment_id WHERE p.id=$1 AND p.environment_id=$2 AND e.org_id=$3`, connection, environment, org).Scan(&registration, &installation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionUnavailable
	}
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM slack_app_registrations WHERE id=$1 AND org_id=$2 FOR NO KEY UPDATE`, registration, org).Scan(&id); err != nil {
		return nil, err
	}
	if installation != nil {
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 AND app_registration_id=$2 FOR NO KEY UPDATE`, *installation, registration).Scan(&id); err != nil {
			return nil, err
		}
	}
	var current *uuid.UUID
	var active bool
	if err = tx.QueryRow(ctx, `SELECT slack_installation_id,revoked_at IS NULL FROM slack_connections WHERE id=$1 FOR NO KEY UPDATE`, connection).Scan(&current, &active); err != nil {
		return nil, err
	}
	// Setup may have completed since optimistic discovery. Retry from the start
	// to acquire the newly discovered installation before the connection gate.
	if (installation == nil) != (current == nil) || current != nil && *current != *installation {
		return nil, ErrConnectionUnavailable
	}

	return &ConnectionDisconnection{registration: registration, installation: installation, connection: connection, environment: environment, org: org, active: active}, nil
}

func (p *ConnectionDisconnection) Apply(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence) error {
	var err error
	if p.installation != nil {
		if err = suppressInstallationPosts(ctx, tx, *p.installation); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_installations SET disconnected_at=COALESCE(disconnected_at,clock_timestamp()),credential_ciphertext=NULL,credential_nonce=NULL,credential_expires_at=NULL,refresh_attempt_id=NULL,refresh_deadline=NULL,last_refresh_attempt_id=NULL WHERE id=$1`, *p.installation)
		if err != nil {
			return err
		}
	}
	if err = slackroute.RevokeConnection(ctx, tx, p.connection); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE slack_app_registrations SET retired_at=COALESCE(retired_at,clock_timestamp()),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, p.registration)
	if err != nil || !p.active {
		return err
	}
	var details audit.Details
	if p.causeAgent != nil {
		details = audit.AgentCleanupDetails{CauseAgentID: p.causeAgent.String(), Reason: "agent_deleted"}
	}
	return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: p.org, EnvironmentID: p.environment, Action: "slack_connection.disconnected", TargetKind: "slack_connection", TargetID: p.connection, Details: details})
}

func (s *CredentialStore) DisconnectConnection(ctx context.Context, actor audit.Evidence, org, user, environment, connection uuid.UUID) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		p, err := LockConnectionDisconnection(ctx, tx, org, user, environment, connection)
		if err != nil {
			return err
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 FOR SHARE`, environment, org).Scan(&id); err != nil {
			return err
		}
		verified, err := connectionAuditActor(ctx, tx, actor, org, user, connection, auth.PermissionAgentConnectionsDelete)
		if err != nil {
			return err
		}
		return p.Apply(ctx, tx, s.auditPolicy, verified)
	})
}
