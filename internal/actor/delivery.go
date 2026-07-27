package actor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/outbox"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type InputReconciler func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
type CloseReconciler func(context.Context, uuid.UUID, uuid.UUID) (bool, error)

const (
	actorDeliveryPollInterval = 250 * time.Millisecond
	actorDeliveryClaimLease   = 30 * time.Second
	actorDeliveryClaimLimit   = int32(32)
)

type DeliveryStore interface {
	ClaimOutboxMessages(context.Context, db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error)
	DeliverOutboxMessage(context.Context, db.DeliverOutboxMessageParams) (db.OutboxMessage, error)
	RetryOutboxMessage(context.Context, db.RetryOutboxMessageParams) (db.OutboxMessage, error)
	DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error)
}

type DeliveryWorker struct {
	log       *slog.Logger
	store     DeliveryStore
	reconcile InputReconciler
	close     CloseReconciler
	workerID  string
	interval  time.Duration
	claimFor  time.Duration
	claimSize int32
	now       func() time.Time
}

func NewDeliveryWorker(
	log *slog.Logger,
	store DeliveryStore,
	reconcile InputReconciler,
	close CloseReconciler,
) (*DeliveryWorker, error) {
	if store == nil {
		return nil, errors.New("Actor input delivery store is required")
	}
	if reconcile == nil {
		return nil, errors.New("Actor input reconciler is required")
	}
	if close == nil {
		return nil, errors.New("Actor close reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeliveryWorker{
		log: log, store: store, reconcile: reconcile, close: close,
		workerID: uuid.Must(uuid.NewV7()).String(), interval: actorDeliveryPollInterval,
		claimFor: actorDeliveryClaimLease, claimSize: actorDeliveryClaimLimit,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Actor input reconciliation delivery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *DeliveryWorker) tick(ctx context.Context) error {
	now := w.now().UTC()
	messages, err := w.store.ClaimOutboxMessages(ctx, db.ClaimOutboxMessagesParams{
		ClaimedBy:      pgtype.Text{String: w.workerID, Valid: true},
		ClaimExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(now.Add(w.claimFor)),
		Lane:           "control", Topics: []string{
			"actor.input.reconcile",
			"actor.close.reconcile",
		}, RowLimit: w.claimSize,
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, message := range messages {
		if err := w.process(ctx, message); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (w *DeliveryWorker) process(ctx context.Context, message db.OutboxMessage) error {
	if message.Topic == "actor.close.reconcile" {
		return w.processClose(ctx, message)
	}
	if message.Topic != "actor.input.reconcile" {
		return w.deadLetter(ctx, message, errors.New("unsupported Actor reconciliation topic"))
	}
	payload, err := decodeActorInputReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	deferred, err := w.reconcile(ctx, payload.environmentID, payload.actorID, payload.recordID)
	if err != nil {
		return w.retry(ctx, message, err, outbox.RetryAfter(message.Attempts))
	}
	if deferred {
		return w.retry(ctx, message, errors.New("Actor input continuation is waiting for Workspace authority"), outbox.RetryAfter(message.Attempts))
	}
	_, err = w.store.DeliverOutboxMessage(ctx, db.DeliverOutboxMessageParams{
		ID: message.ID, ClaimedBy: message.ClaimedBy, ClaimAttempt: message.Attempts,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *DeliveryWorker) processClose(
	ctx context.Context,
	message db.OutboxMessage,
) error {
	payload, err := decodeActorCloseReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	deferred, err := w.close(ctx, payload.environmentID, payload.actorID)
	if err != nil {
		return w.retry(ctx, message, err, outbox.RetryAfter(message.Attempts))
	}
	if deferred {
		return w.retry(
			ctx,
			message,
			errors.New("Actor close is waiting for close authority"),
			outbox.RetryAfter(message.Attempts),
		)
	}
	_, err = w.store.DeliverOutboxMessage(ctx, db.DeliverOutboxMessageParams{
		ID: message.ID, ClaimedBy: message.ClaimedBy, ClaimAttempt: message.Attempts,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *DeliveryWorker) retry(ctx context.Context, message db.OutboxMessage, cause error, after time.Duration) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID: message.ID, ClaimedBy: message.ClaimedBy, ClaimAttempt: message.Attempts,
		AvailableAt: pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(after)),
		LastError:   outbox.Error(cause, "Actor reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

func (w *DeliveryWorker) deadLetter(ctx context.Context, message db.OutboxMessage, cause error) error {
	_, err := w.store.DeadLetterOutboxMessage(ctx, db.DeadLetterOutboxMessageParams{
		ID: message.ID, ClaimedBy: message.ClaimedBy, ClaimAttempt: message.Attempts,
		LastError: outbox.Error(cause, "Actor reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

type actorInputReconcilePayload struct {
	EnvironmentID string `json:"environmentId"`
	ActorID       string `json:"actorId"`
	RecordID      string `json:"recordId"`
	environmentID uuid.UUID
	actorID       uuid.UUID
	recordID      uuid.UUID
}

type actorCloseReconcilePayload struct {
	EnvironmentID string `json:"environmentId"`
	ActorID       string `json:"actorId"`
	environmentID uuid.UUID
	actorID       uuid.UUID
}

func decodeActorInputReconcilePayload(raw []byte) (actorInputReconcilePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value actorInputReconcilePayload
	if err := decoder.Decode(&value); err != nil {
		return actorInputReconcilePayload{}, fmt.Errorf("decode Actor input reconciliation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return actorInputReconcilePayload{}, err
		}
		return actorInputReconcilePayload{}, errors.New("Actor input reconciliation payload contains a trailing JSON value")
	}
	var err error
	if value.environmentID, err = uuid.Parse(value.EnvironmentID); err != nil {
		return actorInputReconcilePayload{}, errors.New("Actor input reconciliation environmentId is invalid")
	}
	if value.actorID, err = uuid.Parse(value.ActorID); err != nil {
		return actorInputReconcilePayload{}, errors.New("Actor input reconciliation actorId is invalid")
	}
	if value.recordID, err = uuid.Parse(value.RecordID); err != nil {
		return actorInputReconcilePayload{}, errors.New("Actor input reconciliation recordId is invalid")
	}
	return value, nil
}

func decodeActorCloseReconcilePayload(
	raw []byte,
) (actorCloseReconcilePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value actorCloseReconcilePayload
	if err := decoder.Decode(&value); err != nil {
		return actorCloseReconcilePayload{}, fmt.Errorf(
			"decode Actor close reconciliation payload: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return actorCloseReconcilePayload{}, err
		}
		return actorCloseReconcilePayload{}, errors.New(
			"Actor close reconciliation payload contains a trailing JSON value",
		)
	}
	var err error
	if value.environmentID, err = uuid.Parse(value.EnvironmentID); err != nil {
		return actorCloseReconcilePayload{}, errors.New(
			"Actor close reconciliation environmentId is invalid",
		)
	}
	if value.actorID, err = uuid.Parse(value.ActorID); err != nil {
		return actorCloseReconcilePayload{}, errors.New(
			"Actor close reconciliation actorId is invalid",
		)
	}
	return value, nil
}
