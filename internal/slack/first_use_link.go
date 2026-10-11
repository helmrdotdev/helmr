package slack

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

var ErrFirstUseLink = errors.New("this Slack linking request has expired or is unavailable; try your action in Slack again")

// A first-use link names a rejected gesture, not an invitation or identity proof.
// Its short lifetime is anchored to the retained rejection receipt. The original
// payload can be disposed without losing the actor or the return destination.
func firstUseURL(config ProjectionConfig, request uuid.UUID, channel string) (string, error) {
	if !config.valid() || request == uuid.Nil() || channel == "" || len(channel) > 100 {
		return "", ErrFirstUseLink
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(request.String() + ":" + channel))
	mac, err := auth.MAC(config.ControlKey, []byte("helmr.slack-link:"), []byte(payload))
	if err != nil {
		return "", err
	}
	target := config.PublicURL.JoinPath("auth/slack/connect")
	target.RawQuery = url.Values{"link": {payload + "." + base64.RawURLEncoding.EncodeToString(mac)}}.Encode()
	return target.String(), nil
}

func decodeFirstUseLink(key []byte, token string) (uuid.UUID, string, error) {
	if len(token) > 240 {
		return uuid.Nil(), "", ErrFirstUseLink
	}
	payload, signature, ok := strings.Cut(token, ".")
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	expected, macErr := auth.MAC(key, []byte("helmr.slack-link:"), []byte(payload))
	if !ok || err != nil || macErr != nil || !hmac.Equal(actual, expected) {
		return uuid.Nil(), "", ErrFirstUseLink
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	request, channel, ok := strings.Cut(string(raw), ":")
	id, idErr := ids.Parse(request)
	if err != nil || idErr != nil || !ok || id == uuid.Nil() || channel == "" || len(channel) > 100 {
		return uuid.Nil(), "", ErrFirstUseLink
	}
	return id, channel, nil
}

type FirstUseLink struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	InstallationID   uuid.UUID `json:"installation_id"`
	TeamID           string    `json:"team_id"`
	SlackUserID      string    `json:"slack_user_id"`
	WorkspaceName    *string   `json:"workspace_name"`
	OrganizationName string    `json:"organization_name"`
	HelmrDisplayName string    `json:"helmr_display_name"`
	ReturnURL        string    `json:"return_url"`
	Linked           bool      `json:"linked"`
}

// ResolveFirstUseLink requires the authenticated Helmr user to belong to the
// intended organization, independently of the browser's currently selected org.
// Slack OIDC must still prove the exact actor before LinkUser can write a link.
func ResolveFirstUseLink(ctx context.Context, pool db.TxBeginner, key []byte, token string, user uuid.UUID) (FirstUseLink, error) {
	request, channel, err := decodeFirstUseLink(key, token)
	if err != nil || user == uuid.Nil() {
		return FirstUseLink{}, ErrFirstUseLink
	}
	var result FirstUseLink
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT i.organization_id,i.id,i.team_id,r.slack_user_id,i.workspace_name,o.name
 FROM slack_requests r JOIN slack_installations i ON i.id=r.installation_id
 JOIN organizations o ON o.id=i.organization_id
 WHERE r.id=$1 AND r.status='rejected' AND r.error='identity_unlinked'
 AND r.finished_at>clock_timestamp()-interval '10 minutes'
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL
 AND r.source_occurred_at>=i.authorized_at AND r.source_occurred_at>=i.connected_at
 AND EXISTS (SELECT 1 FROM agent_publications p JOIN slack_app_registrations a ON a.id=p.slack_app_registration_id WHERE p.slack_installation_id=i.id AND p.revoked_at IS NULL AND a.retired_at IS NULL)
 FOR SHARE OF r,i`, request).Scan(&result.OrganizationID, &result.InstallationID, &result.TeamID, &result.SlackUserID, &result.WorkspaceName, &result.OrganizationName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFirstUseLink
		}
		if err != nil {
			return err
		}
		if err := lockLinkUser(ctx, tx, result.OrganizationID, user); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT u.display_name,EXISTS(SELECT 1 FROM slack_user_links l WHERE l.team_id=$2 AND l.slack_user_id=$3 AND l.user_id=u.id) FROM users u WHERE u.id=$1`, user, result.TeamID, result.SlackUserID).Scan(&result.HelmrDisplayName, &result.Linked)
	})
	if err != nil {
		return FirstUseLink{}, err
	}
	result.ReturnURL = "https://slack.com/app_redirect?" + url.Values{"team": {result.TeamID}, "channel": {channel}}.Encode()
	return result, nil
}
