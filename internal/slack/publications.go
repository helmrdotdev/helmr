package slack

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrPublicationUnavailable = errors.New("the Agent's Slack connection is unavailable")

type PublicationSetup struct {
	ID             uuid.UUID `json:"id"`
	RegistrationID uuid.UUID `json:"registration_id"`
}

// BeginPublication reserves the Agent's sole connection slot, including pending
// setup. A failed setup must be discarded before another can be created.
func BeginPublication(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID) (PublicationSetup, error) {
	setup := PublicationSetup{ID: uuid.NewV7(), RegistrationID: uuid.NewV7()}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 FOR NO KEY UPDATE`, environment, org).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPublicationUnavailable
		}
		if err != nil {
			return err
		}
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN environments e ON e.id=a.environment_id JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(a.environment_id,a.id,e.current_deployment_id) WHERE a.environment_id=$1 AND a.id=$2) AND NOT EXISTS(SELECT 1 FROM agent_publications WHERE environment_id=$1 AND agent_id=$2 AND revoked_at IS NULL)`, environment, agent).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrPublicationUnavailable
		}
		_, err = tx.Exec(ctx, `INSERT INTO slack_app_registrations(id,organization_id,created_by_user_id) VALUES($1,$2,$3)`, setup.RegistrationID, org, user)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,created_by_user_id) VALUES($1,$2,$3,$4,$5)`, setup.ID, environment, agent, setup.RegistrationID, user)
		return err
	})
	if err != nil {
		return PublicationSetup{}, err
	}
	return setup, nil
}

type PublicationView struct {
	ID                    uuid.UUID  `json:"id"`
	RegistrationID        uuid.UUID  `json:"registration_id"`
	InstallationID        *uuid.UUID `json:"installation_id"`
	Status                string     `json:"status"`
	ClientID              *string    `json:"client_id"`
	CredentialsConfigured bool       `json:"credentials_configured"`
	AppID                 *string    `json:"app_id"`
	TeamID                *string    `json:"team_id"`
	WorkspaceName         *string    `json:"workspace_name"`
	AppName               *string    `json:"app_name"`
	AppIconURL            *string    `json:"app_icon_url"`
}

func PublicationAgent(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID, name string) (uuid.UUID, error) {
	var agent uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT a.id FROM agents a JOIN environments e ON e.id=a.environment_id WHERE a.environment_id=$1 AND a.name=$2 AND e.org_id=$3`, environment, name, org).Scan(&agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPublicationUnavailable
		}
		return err
	})
	return agent, err
}

func GetPublication(ctx context.Context, pool db.TxBeginner, org, user, environment, agent uuid.UUID) (*PublicationView, error) {
	var view *PublicationView
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var value PublicationView
		err := tx.QueryRow(ctx, `SELECT p.id,r.id,p.slack_installation_id,
 CASE WHEN i.id IS NULL THEN 'setup_incomplete' WHEN i.disconnected_at IS NOT NULL OR r.retired_at IS NOT NULL THEN 'disconnected' WHEN i.authorization_lost_at IS NOT NULL THEN 'reauthorization_required' ELSE 'connected' END,
 r.client_id,r.credential_revision>0,r.app_id,i.team_id,i.workspace_name,r.app_name,r.app_icon_url
 FROM agent_publications p JOIN environments e ON e.id=p.environment_id JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id LEFT JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.agent_id=$2 AND e.org_id=$3 AND r.organization_id=$3 AND p.revoked_at IS NULL`, environment, agent, org).Scan(&value.ID, &value.RegistrationID, &value.InstallationID, &value.Status, &value.ClientID, &value.CredentialsConfigured, &value.AppID, &value.TeamID, &value.WorkspaceName, &value.AppName, &value.AppIconURL)
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

// DisconnectPublication fences the exact generation, including pending setup.
// Running work remains available through Console/API and is not cancelled.
func (s *CredentialStore) DisconnectPublication(ctx context.Context, org, user, environment, publication uuid.UUID) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var registration uuid.UUID
		var installation *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT p.slack_app_registration_id,p.slack_installation_id FROM agent_publications p JOIN environments e ON e.id=p.environment_id WHERE p.id=$1 AND p.environment_id=$2 AND e.org_id=$3`, publication, environment, org).Scan(&registration, &installation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPublicationUnavailable
		}
		if err != nil {
			return err
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_app_registrations WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE`, registration, org).Scan(&id); err != nil {
			return err
		}
		if installation != nil {
			if err = tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 AND app_registration_id=$2 FOR NO KEY UPDATE`, *installation, registration).Scan(&id); err != nil {
				return err
			}
		}
		var current *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT slack_installation_id FROM agent_publications WHERE id=$1 FOR NO KEY UPDATE`, publication).Scan(&current); err != nil {
			return err
		}
		// Setup may have completed since optimistic discovery. Retry from the start
		// to acquire the newly discovered installation before the Publication gate.
		if (installation == nil) != (current == nil) || current != nil && *current != *installation {
			return ErrPublicationUnavailable
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
		if _, err = tx.Exec(ctx, `UPDATE agent_publications SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE id=$1`, publication); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_app_registrations SET retired_at=COALESCE(retired_at,clock_timestamp()),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, registration)
		return err
	})
}
