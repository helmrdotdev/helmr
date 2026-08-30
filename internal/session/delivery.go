package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/outbox"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type InputReconciler func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
type CloseReconciler func(context.Context, uuid.UUID, uuid.UUID) (bool, error)

const (
	sessionDeliveryPollInterval = 250 * time.Millisecond
	sessionDeliveryClaimLease   = 30 * time.Second
	sessionDeliveryClaimLimit   = int32(32)
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
		return nil, errors.New("session input delivery store is required")
	}
	if reconcile == nil {
		return nil, errors.New("session input reconciler is required")
	}
	if close == nil {
		return nil, errors.New("session close reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeliveryWorker{
		log: log, store: store, reconcile: reconcile, close: close,
		workerID: uuid.NewV7().String(), interval: sessionDeliveryPollInterval,
		claimFor: sessionDeliveryClaimLease, claimSize: sessionDeliveryClaimLimit,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Session input reconciliation delivery failed", "error", err)
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
			"session.input.reconcile",
			"session.close.reconcile",
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
	if message.Topic == "session.close.reconcile" {
		return w.processClose(ctx, message)
	}
	if message.Topic != "session.input.reconcile" {
		return w.deadLetter(ctx, message, errors.New("unsupported session reconciliation topic"))
	}
	payload, err := decodeSessionInputReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	deferred, err := w.reconcile(ctx, payload.environmentID, payload.sessionID, payload.recordID)
	if err != nil {
		return w.retry(ctx, message, err, outbox.RetryAfter(message.Attempts))
	}
	if deferred {
		return w.retry(ctx, message, errors.New("session input continuation is waiting for workspace authority"), outbox.RetryAfter(message.Attempts))
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
	payload, err := decodeSessionCloseReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	deferred, err := w.close(ctx, payload.environmentID, payload.sessionID)
	if err != nil {
		return w.retry(ctx, message, err, outbox.RetryAfter(message.Attempts))
	}
	if deferred {
		return w.retry(
			ctx,
			message,
			errors.New("session close is waiting for close authority"),
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
		LastError:   outbox.Error(cause, "session reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

func (w *DeliveryWorker) deadLetter(ctx context.Context, message db.OutboxMessage, cause error) error {
	_, err := w.store.DeadLetterOutboxMessage(ctx, db.DeadLetterOutboxMessageParams{
		ID: message.ID, ClaimedBy: message.ClaimedBy, ClaimAttempt: message.Attempts,
		LastError: outbox.Error(cause, "session reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

type sessionInputReconcilePayload struct {
	EnvironmentID string `json:"environmentId"`
	SessionID     string `json:"sessionId"`
	RecordID      string `json:"recordId"`
	environmentID uuid.UUID
	sessionID     uuid.UUID
	recordID      uuid.UUID
}

type sessionCloseReconcilePayload struct {
	EnvironmentID string `json:"environmentId"`
	SessionID     string `json:"sessionId"`
	environmentID uuid.UUID
	sessionID     uuid.UUID
}

func decodeSessionInputReconcilePayload(raw []byte) (sessionInputReconcilePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value sessionInputReconcilePayload
	if err := decoder.Decode(&value); err != nil {
		return sessionInputReconcilePayload{}, fmt.Errorf("decode session input reconciliation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return sessionInputReconcilePayload{}, err
		}
		return sessionInputReconcilePayload{}, errors.New("session input reconciliation payload contains a trailing JSON value")
	}
	var err error
	if value.environmentID, err = ids.Parse(value.EnvironmentID); err != nil {
		return sessionInputReconcilePayload{}, errors.New("session input reconciliation environmentId is invalid")
	}
	if value.sessionID, err = ids.Parse(value.SessionID); err != nil {
		return sessionInputReconcilePayload{}, errors.New("session input reconciliation sessionId is invalid")
	}
	if value.recordID, err = ids.Parse(value.RecordID); err != nil {
		return sessionInputReconcilePayload{}, errors.New("session input reconciliation recordId is invalid")
	}
	return value, nil
}

func decodeSessionCloseReconcilePayload(
	raw []byte,
) (sessionCloseReconcilePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value sessionCloseReconcilePayload
	if err := decoder.Decode(&value); err != nil {
		return sessionCloseReconcilePayload{}, fmt.Errorf(
			"decode session close reconciliation payload: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return sessionCloseReconcilePayload{}, err
		}
		return sessionCloseReconcilePayload{}, errors.New(
			"session close reconciliation payload contains a trailing JSON value",
		)
	}
	var err error
	if value.environmentID, err = ids.Parse(value.EnvironmentID); err != nil {
		return sessionCloseReconcilePayload{}, errors.New(
			"session close reconciliation environmentId is invalid",
		)
	}
	if value.sessionID, err = ids.Parse(value.SessionID); err != nil {
		return sessionCloseReconcilePayload{}, errors.New(
			"session close reconciliation sessionId is invalid",
		)
	}
	return value, nil
}
