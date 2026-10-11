package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// linkedHuman retains the live link and membership locks through admission.
// Answering uses current organization membership; work/control operations retain
// the existing core developer role requirement.
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
	var member bool
	err = tx.QueryRow(ctx, `SELECT m.disabled_at IS NULL AND u.disabled_at IS NULL AND (m.role IN ('owner','admin','developer') OR ($3 AND m.role='viewer'))
 FROM org_members m JOIN users u ON u.id=m.user_id JOIN environments e ON e.org_id=m.org_id
 WHERE e.id=$1 AND m.user_id=$2 FOR SHARE OF m,u`, environment, user, answer).Scan(&member)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), "permission_denied", nil
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	if !member {
		return uuid.Nil(), "permission_denied", nil
	}
	return user, "", nil
}
