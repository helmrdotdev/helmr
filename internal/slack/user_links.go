package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrUserLinkDenied = errors.New("the Slack identity link is unavailable for this account")
var ErrUserLinkConflict = errors.New("this Slack identity or Helmr account already has a different link in this workspace; unlink that account first")

type UserLink struct {
	TeamID      string    `json:"team_id"`
	SlackUserID string    `json:"slack_user_id"`
	LinkedAt    time.Time `json:"linked_at"`
}
type LinkWorkspace struct {
	InstallationID uuid.UUID `json:"installation_id"`
	TeamID         string    `json:"team_id"`
	WorkspaceName  *string   `json:"workspace_name"`
}

func lockLinkUser(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT m.disabled_at IS NULL AND u.disabled_at IS NULL FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 AND m.user_id=$2 FOR SHARE OF m,u`, org, user).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return ErrUserLinkDenied
	}
	return err
}

func CheckLinkWorkspace(ctx context.Context, pool db.TxBeginner, org, user, installation uuid.UUID) (LinkWorkspace, error) {
	var v LinkWorkspace
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		// Linking alone grants no org or publication authority. Read the connection
		// without acquiring admission's installation gate while holding user locks.
		err := tx.QueryRow(ctx, `SELECT id,team_id,workspace_name FROM slack_installations WHERE id=$1 AND organization_id=$2 AND disconnected_at IS NULL AND authorization_lost_at IS NULL`, installation, org).Scan(&v.InstallationID, &v.TeamID, &v.WorkspaceName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserLinkDenied
		}
		return err
	})
	return v, err
}
func ListLinkWorkspaces(ctx context.Context, pool db.TxBeginner, org, user, before uuid.UUID, limit int) ([]LinkWorkspace, error) {
	result := []LinkWorkspace{}
	if limit < 1 || limit > 500 {
		return nil, ErrUserLinkDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,team_id,workspace_name FROM slack_installations WHERE organization_id=$1 AND disconnected_at IS NULL AND authorization_lost_at IS NULL AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR id<$2) ORDER BY id DESC LIMIT $3`, org, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v LinkWorkspace
			if err = rows.Scan(&v.InstallationID, &v.TeamID, &v.WorkspaceName); err != nil {
				return err
			}
			result = append(result, v)
		}
		return rows.Err()
	})
	return result, err
}
func ListUserLinks(ctx context.Context, pool db.TxBeginner, org, user uuid.UUID, after string, limit int) ([]UserLink, error) {
	result := []UserLink{}
	if limit < 1 || limit > 500 || len(after) > 100 {
		return nil, ErrUserLinkDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT team_id,slack_user_id,linked_at FROM slack_user_links WHERE user_id=$1 AND team_id>$2 ORDER BY team_id LIMIT $3`, user, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v UserLink
			if err = rows.Scan(&v.TeamID, &v.SlackUserID, &v.LinkedAt); err != nil {
				return err
			}
			result = append(result, v)
		}
		return rows.Err()
	})
	return result, err
}

// LinkUser is called only after OpenID proof and explicit browser confirmation.
// Uniqueness never performs a merge or replacement, and exact retries preserve
// the original link time used to reject older Slack gestures.
func LinkUser(ctx context.Context, pool db.TxBeginner, org, user, installation uuid.UUID, identity UserIdentity) error {
	if identity.TeamID == "" || identity.SlackUserID == "" || len(identity.TeamID) > 100 || len(identity.SlackUserID) > 100 {
		return ErrUserLinkDenied
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_installations WHERE id=$1 AND organization_id=$2 AND team_id=$3 AND disconnected_at IS NULL AND authorization_lost_at IS NULL)`, installation, org, identity.TeamID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrUserLinkDenied
		}
		tag, err := tx.Exec(ctx, `INSERT INTO slack_user_links(team_id,slack_user_id,user_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, identity.TeamID, identity.SlackUserID, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var exact bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_user_links WHERE team_id=$1 AND slack_user_id=$2 AND user_id=$3)`, identity.TeamID, identity.SlackUserID, user).Scan(&exact)
		if err != nil {
			return err
		}
		if !exact {
			return ErrUserLinkConflict
		}
		return nil
	})
}

// An exact Slack user ID prevents a stale settings page from removing a newer
// replacement link for the same workspace. Historical admitted requests remain.
func UnlinkUser(ctx context.Context, pool db.TxBeginner, org, user uuid.UUID, team, slackUser string) error {
	if team == "" || slackUser == "" || len(team) > 100 || len(slackUser) > 100 {
		return ErrUserLinkDenied
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM slack_user_links WHERE team_id=$1 AND slack_user_id=$2 AND user_id=$3`, team, slackUser, user)
		return err
	})
}
