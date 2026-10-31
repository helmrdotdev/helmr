package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"uuid"
)

var ErrAgentDeleted = errors.New("agent is deleted; restore it before execution")

func requireAgentAvailable(ctx context.Context, q db.DBTX, env, agent uuid.UUID) error {
	var deleted bool
	if err := q.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM agents WHERE environment_id=$1 AND id=$2`, env, agent).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return ErrAgentDeleted
	}
	return nil
}
