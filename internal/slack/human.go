package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
)

// linkedHuman retains the live link and membership locks through admission.
// Slack input requires current environment run-operation access.
func linkedHuman(ctx context.Context, tx pgx.Tx, team, actor string, environment uuid.UUID, source time.Time, answer bool) (uuid.UUID, string, error) {
	var user uuid.UUID
	var linked time.Time
	err := tx.QueryRow(ctx, `SELECT user_id,linked_at FROM slack_user_links WHERE team_id=$1 AND slack_user_id=$2 FOR SHARE`, team, actor).Scan(&user, &linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), "identity_unlinked", nil
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	if source.Before(linked) {
		return uuid.Nil(), "identity_unlinked", nil
	}
	err = access.Check(ctx, tx, "user", user, environment, uuid.Nil(), auth.CapabilityRunsOperate)
	if errors.Is(err, access.ErrDenied) {
		return uuid.Nil(), "permission_denied", nil
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	return user, "", nil
}
