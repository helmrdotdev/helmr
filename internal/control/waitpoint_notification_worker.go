package control

import (
	"context"
	"errors"
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
	defaultWaitpointNotificationReconcileEvery = 30 * time.Second
	defaultWaitpointNotificationBatchSize      = int32(100)
	waitpointDeliveryMessageType               = "waitpoint.delivery"
	asyncMessageVersionV0                      = 0
)

type WaitpointNotificationWorker struct {
	server         *Server
	queue          asyncbus.Subscriber
	reconcileEvery time.Duration
	batchSize      int32
}

func NewWaitpointNotificationWorker(log *slog.Logger, store db.Querier, queue asyncbus.Subscriber, opts ...Option) (*WaitpointNotificationWorker, error) {
	if store == nil {
		return nil, errors.New("waitpoint notification store is required")
	}
	server := &Server{log: log, db: store, mailer: unconfiguredEmailSender{}}
	for _, opt := range opts {
		opt(server)
	}
	if server.log == nil {
		server.log = slog.Default()
	}
	return &WaitpointNotificationWorker{
		server:         server,
		queue:          queue,
		reconcileEvery: defaultWaitpointNotificationReconcileEvery,
		batchSize:      defaultWaitpointNotificationBatchSize,
	}, nil
}

func (w *WaitpointNotificationWorker) Run(ctx context.Context) error {
	if w.queue == nil {
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

func (w *WaitpointNotificationWorker) receiveLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		messages, err := w.queue.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.server.log.Warn("receive waitpoint notification signals failed", "error", err)
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
				w.server.log.Warn("invalid async bus message envelope", "error", received.DecodeErr)
				continue
			}
			deliveryID := waitpointDeliveryIDFromAsyncMessage(received.Message)
			if deliveryID == uuid.Nil {
				w.server.log.Warn("unsupported async bus message", "type", received.Message.Type, "version", received.Message.Version, "id", received.Message.ID)
				continue
			}
			if err := w.server.SendQueuedWaitpointDelivery(ctx, deliveryID); err != nil {
				w.server.log.Warn("send waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				continue
			}
			if err := w.queue.Delete(ctx, received); err != nil {
				w.server.log.Warn("delete async bus message failed", "error", err)
			}
		}
	}
}

func (w *WaitpointNotificationWorker) reconcileLoop(ctx context.Context, sendDirect bool) error {
	ticker := time.NewTicker(w.reconcileEvery)
	defer ticker.Stop()
	for {
		if err := w.reconcileOnce(ctx, sendDirect); err != nil {
			w.server.log.Warn("reconcile waitpoint notifications failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *WaitpointNotificationWorker) reconcileOnce(ctx context.Context, sendDirect bool) error {
	staleBefore := pgtype.Timestamptz{Time: time.Now().UTC().Add(-waitpointDeliveryClaimStale), Valid: true}
	if err := w.server.db.RequeueStaleSendingWaitpointDeliveries(ctx, db.RequeueStaleSendingWaitpointDeliveriesParams{
		StaleBefore: staleBefore,
		MaxAttempts: waitpointDeliveryMaxAttempts,
	}); err != nil {
		return err
	}
	for {
		deliveries, err := w.server.db.ListDueWaitpointDeliveries(ctx, w.batchSize)
		if err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return nil
		}
		for _, delivery := range deliveries {
			deliveryID := ids.MustFromPG(delivery.ID)
			if sendDirect || w.queue == nil {
				if err := w.server.SendQueuedWaitpointDelivery(ctx, deliveryID); err != nil {
					w.server.log.Warn("send due waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				}
				continue
			}
			if _, err := w.queue.Publish(ctx, waitpointDeliveryAsyncMessage(delivery)); err != nil {
				w.server.log.Warn("enqueue due waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
				continue
			}
			w.server.markWaitpointDeliverySignaled(ctx, delivery, time.Now().UTC().Add(w.reconcileEvery))
		}
		if !sendDirect && w.queue != nil {
			return nil
		}
		if int32(len(deliveries)) < w.batchSize {
			return nil
		}
	}
}

func waitpointDeliveryAsyncMessage(delivery db.WaitpointDelivery) asyncbus.Message {
	return asyncbus.Message{
		Type:        waitpointDeliveryMessageType,
		Version:     asyncMessageVersionV0,
		ID:          ids.MustFromPG(delivery.ID).String(),
		FairGroupID: waitpointDeliveryFairGroupID(delivery.OrgID),
	}
}

func waitpointDeliveryIDFromAsyncMessage(message asyncbus.Message) uuid.UUID {
	if message.Type != waitpointDeliveryMessageType || message.Version != asyncMessageVersionV0 {
		return uuid.Nil
	}
	deliveryID, err := uuid.Parse(message.ID)
	if err != nil {
		return uuid.Nil
	}
	return deliveryID
}

func waitpointDeliveryFairGroupID(orgID pgtype.UUID) string {
	if !orgID.Valid {
		return waitpointDeliveryMessageType
	}
	return "org:" + ids.MustFromPG(orgID).String()
}
