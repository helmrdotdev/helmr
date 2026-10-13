package slack

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type connectionAuthorization struct {
	registration uuid.UUID
	installation *uuid.UUID
	revision     int64
	clientID     string
	app          *string
}

func (s *CredentialStore) connectionAuthorization(ctx context.Context, org, user, connection uuid.UUID) (connectionAuthorization, error) {
	var value connectionAuthorization
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT r.id,p.slack_installation_id,r.credential_revision,r.client_id,r.app_id FROM slack_connections p JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id JOIN environments e ON e.id=p.environment_id WHERE p.id=$1 AND e.org_id=$2 AND r.org_id=$2 AND p.revoked_at IS NULL AND r.retired_at IS NULL AND r.credential_revision>0`, connection, org).Scan(&value.registration, &value.installation, &value.revision, &value.clientID, &value.app)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		return err
	})
	return value, err
}

func (s *CredentialStore) ConnectionAuthorizationURL(ctx context.Context, org, user, connection uuid.UUID, state, redirect string) (string, error) {
	value, err := s.connectionAuthorization(ctx, org, user, connection)
	if err != nil {
		return "", err
	}
	oauth := OAuthClient{clientID: value.clientID}
	return oauth.AuthorizationURL(state, redirect), nil
}

// AuthorizeConnection performs remote exchange outside SQL locks, then verifies
// the unchanged registration and connection slot before attaching the grant.
func (s *CredentialStore) AuthorizeConnection(ctx context.Context, actor audit.Actor, org, user, connection uuid.UUID, code, redirect string, transport http.RoundTripper) (uuid.UUID, error) {
	if code == "" || len(code) > 4096 || redirect == "" {
		return uuid.Nil(), ErrOAuthAuthorization
	}
	observed, err := s.connectionAuthorization(ctx, org, user, connection)
	if err != nil {
		return uuid.Nil(), err
	}
	oauth, err := s.RegistrationOAuth(ctx, observed.registration, transport)
	if err != nil {
		return uuid.Nil(), err
	}
	result := oauth.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}})
	if result.code != "" || result.grant.app == "" || result.grant.team == "" || result.grant.bot == "" {
		return uuid.Nil(), ErrOAuthAuthorization
	}
	for _, required := range slackroute.RequiredScopes() {
		found := false
		for _, scope := range result.grant.scopes {
			if scope == required {
				found = true
				break
			}
		}
		if !found {
			return uuid.Nil(), ErrOAuthAuthorization
		}
	}
	return s.attachConnectionGrant(ctx, actor, org, user, connection, observed, result.grant)
}

func (s *CredentialStore) attachConnectionGrant(ctx context.Context, actor audit.Actor, org, user, connection uuid.UUID, observed connectionAuthorization, grant oauthGrant) (uuid.UUID, error) {
	installation := uuid.NewV7()
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var revision int64
		var app *string
		err := tx.QueryRow(ctx, `SELECT credential_revision,app_id FROM slack_app_registrations WHERE id=$1 AND org_id=$2 AND retired_at IS NULL FOR NO KEY UPDATE`, observed.registration, org).Scan(&revision, &app)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		if err != nil {
			return err
		}
		if revision != observed.revision || app != nil && *app != grant.app {
			return ErrConnectionUnavailable
		}
		// Existing installation identities precede the connection lock.
		var installedApp, team, bot string
		var tokenRevision int64 = 1
		if observed.installation != nil {
			installation = *observed.installation
			err = tx.QueryRow(ctx, `SELECT app_id,team_id,bot_user_id,credential_revision FROM slack_installations WHERE id=$1 AND app_registration_id=$2 AND org_id=$3 AND disconnected_at IS NULL FOR NO KEY UPDATE`, installation, observed.registration, org).Scan(&installedApp, &team, &bot, &tokenRevision)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConnectionUnavailable
			}
			if err != nil {
				return err
			}
			if installedApp != grant.app || team != grant.team || bot != grant.bot {
				return ErrConnectionUnavailable
			}
			tokenRevision++
		}
		var current *uuid.UUID
		var environment uuid.UUID
		err = tx.QueryRow(ctx, `SELECT environment_id,slack_installation_id FROM slack_connections WHERE id=$1 AND slack_app_registration_id=$2 AND revoked_at IS NULL FOR NO KEY UPDATE`, connection, observed.registration).Scan(&environment, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionUnavailable
		}
		if err != nil {
			return err
		}
		if (current == nil) != (observed.installation == nil) || current != nil && *current != *observed.installation {
			return ErrConnectionUnavailable
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 FOR SHARE`, environment, org).Scan(&id); err != nil {
			return ErrConnectionUnavailable
		}
		ciphertext, nonce, err := s.seal(org, installation, tokenRevision, grant.bundle)
		if err != nil {
			return err
		}
		if current == nil {
			_, err = tx.Exec(ctx, `INSERT INTO slack_installations(id,org_id,app_registration_id,app_id,team_id,bot_user_id,workspace_name,credential_revision,credential_ciphertext,credential_nonce,credential_expires_at,granted_scopes,connected_by_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, installation, org, observed.registration, grant.app, grant.team, grant.bot, grant.workspace, tokenRevision, ciphertext, nonce, grant.bundle.ExpiresAt, grant.scopes, user)
		} else {
			if err = suppressInstallationPosts(ctx, tx, installation); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE slack_installations SET workspace_name=$2,credential_revision=$3,credential_ciphertext=$4,credential_nonce=$5,credential_expires_at=$6,granted_scopes=$7,authorized_at=clock_timestamp(),authorization_lost_at=NULL,refresh_attempt_id=NULL,refresh_deadline=NULL,last_refresh_attempt_id=NULL,refresh_error=NULL,refresh_next_at=clock_timestamp() WHERE id=$1`, installation, grant.workspace, tokenRevision, ciphertext, nonce, grant.bundle.ExpiresAt, grant.scopes)
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_app_registrations SET app_id=$2 WHERE id=$1 AND app_id IS NULL`, observed.registration, grant.app); err != nil {
			return err
		}
		if err = slackroute.AttachInstallation(ctx, tx, connection, installation); err != nil {
			return err
		}
		return audit.Append(ctx, tx, audit.Event{Actor: actor, OrgID: org, EnvironmentID: environment, Action: "slack_connection.authorized", TargetKind: "slack_connection", TargetID: connection, Details: audit.PermissionsDetails{Permissions: grant.scopes}})
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return installation, nil
}
