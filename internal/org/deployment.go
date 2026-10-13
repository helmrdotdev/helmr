package org

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// SetCurrentDeployment records a promotion in the caller's transaction. The
// caller holds the Environment lock, has checked the Deployment under its share
// lock, and commits trigger activation and image refresh policy with this write.
func SetCurrentDeployment(ctx context.Context, tx pgx.Tx, environment, deployment uuid.UUID, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE environments SET current_deployment_id=$2,updated_at=$3 WHERE id=$1`, environment, deployment, at)
	return err
}
