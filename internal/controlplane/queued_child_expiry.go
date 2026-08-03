package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	rundomain "github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
)

const (
	queuedChildExpiryInterval = 250 * time.Millisecond
	queuedChildExpiryLimit    = int32(100)
)

type queuedChildExpiryWorkflow struct {
	server *Server
}

func (w queuedChildExpiryWorkflow) run(ctx context.Context) {
	ticker := time.NewTicker(queuedChildExpiryInterval)
	defer ticker.Stop()
	for {
		if err := w.expire(ctx, queuedChildExpiryLimit); err != nil &&
			ctx.Err() == nil && w.server != nil && w.server.log != nil {
			w.server.log.Error("expire queued child Runs failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w queuedChildExpiryWorkflow) expire(ctx context.Context, limit int32) error {
	if w.server == nil || w.server.db == nil {
		return errors.New("queued child expiry storage is required")
	}
	candidates, err := w.server.db.ListExpiredParentOwnedChildRuns(ctx, limit)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := w.server.inTx(ctx, func(work *txWork) error {
			tx, ok := work.tx.(pgx.Tx)
			if !ok {
				return errors.New("queued child expiry transaction does not expose PostgreSQL authority")
			}
			_, err := rundomain.ExpireParentOwnedChild(
				ctx,
				tx,
				rundomain.ChildExpiryRequest{
					OrgID:         pgvalue.MustUUIDValue(candidate.OrgID),
					ProjectID:     pgvalue.MustUUIDValue(candidate.ProjectID),
					EnvironmentID: pgvalue.MustUUIDValue(candidate.EnvironmentID),
					ParentRunID:   pgvalue.MustUUIDValue(candidate.ParentRunID),
					ChildRunID:    pgvalue.MustUUIDValue(candidate.ID),
				},
			)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
