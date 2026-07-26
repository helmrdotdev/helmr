package runadmission

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type RunWaitDeadlineReconcile func(context.Context, int32) (int, error)

type RunWaitDeadlineDeliveryWorker struct {
	log           *slog.Logger
	timerDue      RunWaitDeadlineReconcile
	tokenTimeouts RunWaitDeadlineReconcile
	actorTimeouts RunWaitDeadlineReconcile
	interval      time.Duration
	batchSize     int32
}

func NewRunWaitDeadlineDeliveryWorker(
	log *slog.Logger,
	timerDue RunWaitDeadlineReconcile,
	tokenTimeouts RunWaitDeadlineReconcile,
	actorTimeouts RunWaitDeadlineReconcile,
) (*RunWaitDeadlineDeliveryWorker, error) {
	if timerDue == nil {
		return nil, errors.New("timer Wait reconciler is required")
	}
	if tokenTimeouts == nil {
		return nil, errors.New("Token Wait timeout reconciler is required")
	}
	if actorTimeouts == nil {
		return nil, errors.New("Actor input Wait timeout reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RunWaitDeadlineDeliveryWorker{
		log: log, timerDue: timerDue,
		tokenTimeouts: tokenTimeouts, actorTimeouts: actorTimeouts,
		interval: tokenDeliveryPollInterval, batchSize: tokenReconcileBatchLimit,
	}, nil
}

func (w *RunWaitDeadlineDeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Run Wait deadline reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *RunWaitDeadlineDeliveryWorker) tick(ctx context.Context) error {
	if _, err := w.timerDue(ctx, w.batchSize); err != nil {
		return err
	}
	if _, err := w.tokenTimeouts(ctx, w.batchSize); err != nil {
		return err
	}
	if _, err := w.actorTimeouts(ctx, w.batchSize); err != nil {
		return err
	}
	return nil
}
