package waitpoint

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/asyncbus"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"
)

const (
	defaultNotificationReconcileEvery = 30 * time.Second
	defaultNotificationBatchSize      = int32(100)
	deliveryMessageType               = "waitpoint.delivery"
	asyncMessageVersionV0             = 0
)

type Worker struct {
	notifier       *Notifier
	subscriber     Subscriber
	log            *slog.Logger
	reconcileEvery time.Duration
	batchSize      int32
}

func NewWorker(notifier *Notifier, subscriber Subscriber, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		notifier:       notifier,
		subscriber:     subscriber,
		log:            log,
		reconcileEvery: defaultNotificationReconcileEvery,
		batchSize:      defaultNotificationBatchSize,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.subscriber == nil {
		return w.reconcileLoop(ctx, true)
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return w.receiveLoop(groupCtx)
	})
	group.Go(func() error {
		return w.reconcileLoop(groupCtx, false)
	})
	return group.Wait()
}

func (w *Worker) receiveLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		messages, err := w.subscriber.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.log.Warn("receive waitpoint notification signals failed", "error", err)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		for _, received := range messages {
			if received.DecodeErr != nil {
				w.log.Warn("invalid async bus message envelope", "error", received.DecodeErr)
				continue
			}
			deliveryID := deliveryIDFromAsyncMessage(received.Message)
			if deliveryID == uuid.Nil {
				w.log.Warn("unsupported async bus message", "type", received.Message.Type, "version", received.Message.Version, "id", received.Message.ID)
				continue
			}
			if err := w.notifier.SendQueuedDelivery(ctx, deliveryID); err != nil {
				w.log.Warn("send waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				continue
			}
			if err := w.subscriber.Delete(ctx, received); err != nil {
				w.log.Warn("delete async bus message failed", "error", err)
			}
		}
	}
}

func (w *Worker) reconcileLoop(ctx context.Context, sendDirect bool) error {
	ticker := time.NewTicker(w.reconcileEvery)
	defer ticker.Stop()
	for {
		if err := w.reconcileOnce(ctx, sendDirect); err != nil {
			w.log.Warn("reconcile waitpoint notifications failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) reconcileOnce(ctx context.Context, sendDirect bool) error {
	staleBefore := pgtype.Timestamptz{Time: time.Now().UTC().Add(-deliveryClaimStale), Valid: true}
	if err := w.notifier.store.RequeueStaleSendingWaitpointDeliveries(ctx, db.RequeueStaleSendingWaitpointDeliveriesParams{
		StaleBefore: staleBefore,
		MaxAttempts: deliveryMaxAttempts,
	}); err != nil {
		return err
	}
	for {
		deliveries, err := w.notifier.store.ListDueWaitpointDeliveries(ctx, w.batchSize)
		if err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return nil
		}
		for _, delivery := range deliveries {
			deliveryID := ids.MustFromPG(delivery.ID)
			if sendDirect || w.notifier.publisher == nil {
				if err := w.notifier.SendQueuedDelivery(ctx, deliveryID); err != nil {
					w.log.Warn("send due waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				}
				continue
			}
			if _, err := w.notifier.publisher.Publish(ctx, deliveryAsyncMessage(delivery)); err != nil {
				w.log.Warn("enqueue due waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				continue
			}
			w.notifier.markDeliverySignaled(ctx, delivery, time.Now().UTC().Add(w.reconcileEvery))
		}
		if !sendDirect && w.notifier.publisher != nil {
			return nil
		}
		if int32(len(deliveries)) < w.batchSize {
			return nil
		}
	}
}

func deliveryAsyncMessage(delivery db.WaitpointDelivery) asyncbus.Message {
	return asyncbus.Message{
		Type:        deliveryMessageType,
		Version:     asyncMessageVersionV0,
		ID:          ids.MustFromPG(delivery.ID).String(),
		FairGroupID: deliveryFairGroupID(delivery.OrgID),
	}
}

func deliveryIDFromAsyncMessage(message asyncbus.Message) uuid.UUID {
	if message.Type != deliveryMessageType || message.Version != asyncMessageVersionV0 {
		return uuid.Nil
	}
	deliveryID, err := uuid.Parse(message.ID)
	if err != nil {
		return uuid.Nil
	}
	return deliveryID
}

func deliveryFairGroupID(orgID pgtype.UUID) string {
	if !orgID.Valid {
		return deliveryMessageType
	}
	return "org:" + ids.MustFromPG(orgID).String()
}
