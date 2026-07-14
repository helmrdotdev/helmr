package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type CheckpointDiscovery interface {
	ListDueRunCheckpointWaits(context.Context, int32) ([]db.ListDueRunCheckpointWaitsRow, error)
}

type CheckpointAuthority interface {
	RequestCheckpoint(context.Context, db.ClaimRunCheckpointWaitParams) (db.ClaimRunCheckpointWaitRow, error)
}

// CheckpointReconciler is the sole owner that advances hot waits into a
// checkpoint request. Workers only acknowledge the durable request version;
// they never create or increment it.
type CheckpointReconciler struct {
	discovery CheckpointDiscovery
	authority CheckpointAuthority
	wakes     WorkerWakePublisher
	every     time.Duration
	limit     int32
	log       *slog.Logger
}

func NewCheckpointReconciler(discovery CheckpointDiscovery, authority CheckpointAuthority, wakes WorkerWakePublisher, log *slog.Logger) (*CheckpointReconciler, error) {
	if discovery == nil || authority == nil || wakes == nil {
		return nil, errors.New("checkpoint discovery, authority, and wake publisher are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &CheckpointReconciler{discovery: discovery, authority: authority, wakes: wakes,
		every: time.Second, limit: 100, log: log}, nil
}

func (r *CheckpointReconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		if err := r.ReconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("checkpoint intent reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *CheckpointReconciler) ReconcileOnce(ctx context.Context) error {
	rows, err := r.discovery.ListDueRunCheckpointWaits(ctx, r.limit)
	if err != nil {
		return fmt.Errorf("list due checkpoint waits: %w", err)
	}
	var problems []error
	for _, row := range rows {
		claimed, err := r.authority.RequestCheckpoint(ctx, db.ClaimRunCheckpointWaitParams(row))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			problems = append(problems, err)
			continue
		}
		if err == nil {
			if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "checkpoint", WorkerID: claimed.WorkerInstanceID,
				WorkerEpoch: claimed.WorkerEpoch, RuntimeID: claimed.RuntimeInstanceID, AuthorityID: claimed.ID,
				RequestVersion: claimed.CheckpointRequestVersion}); err != nil {
				problems = append(problems, fmt.Errorf("publish checkpoint wake: %w", err))
			}
		}
	}
	return errors.Join(problems...)
}
