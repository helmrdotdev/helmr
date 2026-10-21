package slack

import (
	"context"
	"errors"
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
func BeginConnection(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID) (ConnectionSetup, error) {
	setup := ConnectionSetup{ID: uuid.NewV7(), RegistrationID: uuid.NewV7()}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
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
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN environments e ON e.id=a.environment_id JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(a.environment_id,a.id,e.current_deployment_id) WHERE a.environment_id=$1 AND a.id=$2 AND d.input_kind='text') AND NOT EXISTS(SELECT 1 FROM slack_connections WHERE environment_id=$1 AND agent_id=$2 AND revoked_at IS NULL)`, environment, agent).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrConnectionUnavailable
		}
		_, err = tx.Exec(ctx, `INSERT INTO slack_app_registrations(id,org_id,created_by_user_id) VALUES($1,$2,$3)`, setup.RegistrationID, org, user)
		if err != nil {
			return err
		}
		return slackroute.CreateConnection(ctx, tx, setup.ID, environment, agent, setup.RegistrationID, user)
	})
	if err != nil {
		return ConnectionSetup{}, err
	}
	return setup, nil
}

type ConnectionView struct {
	ID                       uuid.UUID  `json:"id"`
	RegistrationID           uuid.UUID  `json:"registration_id"`
	InstallationID           *uuid.UUID `json:"installation_id"`
	Status                   string     `json:"status"`
	ClientID                 *string    `json:"client_id"`
	DirectMessagesAuthorized bool       `json:"direct_messages_authorized"`
	CredentialsConfigured    bool       `json:"credentials_configured"`
	AppID                    *string    `json:"app_id"`
	TeamID                   *string    `json:"team_id"`
	WorkspaceName            *string    `json:"workspace_name"`
	AppName                  *string    `json:"app_name"`
	AppIconURL               *string    `json:"app_icon_url"`
}

func ConnectionAgent(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID, name string) (uuid.UUID, error) {
	var agent uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT a.id FROM agents a JOIN environments e ON e.id=a.environment_id WHERE a.environment_id=$1 AND a.key=$2 AND e.org_id=$3`, environment, name, org).Scan(&agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		return err
	})
	return agent, err
}

func GetConnection(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID) (*ConnectionView, error) {
	var view *ConnectionView
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var value ConnectionView
		err := tx.QueryRow(ctx, `SELECT p.id,r.id,p.slack_installation_id,
 CASE WHEN i.id IS NULL THEN 'setup_incomplete' WHEN i.disconnected_at IS NOT NULL OR r.retired_at IS NOT NULL THEN 'disconnected' WHEN i.authorization_lost_at IS NOT NULL THEN 'reauthorization_required' ELSE 'connected' END,
 r.client_id,r.credential_revision>0,r.app_id,i.team_id,i.workspace_name,r.app_name,r.app_icon_url,COALESCE(i.granted_scopes @> $4::text[],false)
 FROM slack_connections p JOIN environments e ON e.id=p.environment_id JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id LEFT JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.agent_id=$2 AND e.org_id=$3 AND r.org_id=$3 AND p.revoked_at IS NULL`, environment, agent, org, slackroute.DirectMessageScopes()).Scan(&value.ID, &value.RegistrationID, &value.InstallationID, &value.Status, &value.ClientID, &value.CredentialsConfigured, &value.AppID, &value.TeamID, &value.WorkspaceName, &value.AppName, &value.AppIconURL, &value.DirectMessagesAuthorized)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		view = &value
		return nil
	})
	return view, err
}

// DisconnectConnection fences the exact generation, including pending setup.
// Running work remains available through Console/API and is not cancelled.
func (s *CredentialStore) DisconnectConnection(ctx context.Context, actor audit.Actor, org, user, environment, connection uuid.UUID) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var registration uuid.UUID
		var installation *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT p.slack_app_registration_id,p.slack_installation_id FROM slack_connections p JOIN environments e ON e.id=p.environment_id WHERE p.id=$1 AND p.environment_id=$2 AND e.org_id=$3`, connection, environment, org).Scan(&registration, &installation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		if err != nil {
			return err
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_app_registrations WHERE id=$1 AND org_id=$2 FOR NO KEY UPDATE`, registration, org).Scan(&id); err != nil {
			return err
		}
		if installation != nil {
			if err = tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 AND app_registration_id=$2 FOR NO KEY UPDATE`, *installation, registration).Scan(&id); err != nil {
				return err
			}
		}
		var current *uuid.UUID
		var active bool
		if err = tx.QueryRow(ctx, `SELECT slack_installation_id,revoked_at IS NULL FROM slack_connections WHERE id=$1 FOR NO KEY UPDATE`, connection).Scan(&current, &active); err != nil {
			return err
		}
		// Setup may have completed since optimistic discovery. Retry from the start
		// to acquire the newly discovered installation before the connection gate.
		if (installation == nil) != (current == nil) || current != nil && *current != *installation {
			return ErrConnectionUnavailable
		}
		if err = tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 FOR SHARE`, environment, org).Scan(&id); err != nil {
			return err
		}
		if installation != nil {
			if err = suppressInstallationPosts(ctx, tx, *installation); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE slack_installations SET disconnected_at=COALESCE(disconnected_at,clock_timestamp()),credential_ciphertext=NULL,credential_nonce=NULL,credential_expires_at=NULL,refresh_attempt_id=NULL,refresh_deadline=NULL,last_refresh_attempt_id=NULL WHERE id=$1`, *installation)
			if err != nil {
				return err
			}
		}
		if err = slackroute.RevokeConnection(ctx, tx, connection); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_app_registrations SET retired_at=COALESCE(retired_at,clock_timestamp()),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, registration)
		if err != nil || !active {
			return err
		}
		return audit.Append(ctx, tx, audit.Event{Actor: actor, OrgID: org, EnvironmentID: environment, Action: "slack_connection.disconnected", TargetKind: "slack_connection", TargetID: connection})
	})
}
