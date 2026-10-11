package slack

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

var errInstallationAuthority = errors.New("management of Slack installations is unavailable")

func lockInstallationManager(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin')
 FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 AND m.user_id=$2 FOR SHARE OF m,u`, org, user).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allowed) {
		return errInstallationAuthority
	}
	return err
}
