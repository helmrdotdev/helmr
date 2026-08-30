package token

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

const (
	tokenDeliveryPollInterval = 250 * time.Millisecond
	tokenDeliveryClaimLease   = 30 * time.Second
	tokenDeliveryClaimLimit   = int32(32)
	tokenReconcileBatchLimit  = int32(100)
)

type DeliveryStore interface {
	ExpireDueTokens(context.Context, db.ExpireDueTokensParams) ([]db.ExpireDueTokensRow, error)
	ExpireDuePublicAccessTokens(context.Context, int32) ([]db.PublicAccessToken, error)
	ClaimOutboxMessages(context.Context, db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error)
	DeliverOutboxMessage(context.Context, db.DeliverOutboxMessageParams) (db.OutboxMessage, error)
	RetryOutboxMessage(context.Context, db.RetryOutboxMessageParams) (db.OutboxMessage, error)
	DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error)
}

type ReconcileBatch func(context.Context, uuid.UUID, uuid.UUID, int32) (WaitBatch, error)

type DeliveryWorker struct {
	log       *slog.Logger
	store     DeliveryStore
	reconcile ReconcileBatch
	workerID  string
	interval  time.Duration
	claimFor  time.Duration
	claimSize int32
	batchSize int32
	now       func() time.Time
}

func NewDeliveryWorker(log *slog.Logger, store DeliveryStore, reconcile ReconcileBatch) (*DeliveryWorker, error) {
	if store == nil {
		return nil, errors.New("token reconciliation delivery store is required")
	}
	if reconcile == nil {
		return nil, errors.New("token wait reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeliveryWorker{
		log:       log,
		store:     store,
		reconcile: reconcile,
		workerID:  uuid.NewV7().String(),
		interval:  tokenDeliveryPollInterval,
		claimFor:  tokenDeliveryClaimLease,
		claimSize: tokenDeliveryClaimLimit,
		batchSize: tokenReconcileBatchLimit,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Token reconciliation delivery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *DeliveryWorker) tick(ctx context.Context) error {
	if _, err := w.store.ExpireDueTokens(ctx, db.ExpireDueTokensParams{
		OutboxMessageIds: pgvalue.NewUUIDv7Batch(w.batchSize),
		LimitCount:       w.batchSize,
	}); err != nil {
		return fmt.Errorf("expire due tokens: %w", err)
	}
	if _, err := w.store.ExpireDuePublicAccessTokens(ctx, w.batchSize); err != nil {
		return fmt.Errorf("expire due token public access credentials: %w", err)
	}
	now := w.now().UTC()
	messages, err := w.store.ClaimOutboxMessages(ctx, db.ClaimOutboxMessagesParams{
		ClaimedBy:      pgtype.Text{String: w.workerID, Valid: true},
		ClaimExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(now.Add(w.claimFor)),
		Lane:           "control",
		Topics:         []string{"token.reconcile"},
		RowLimit:       w.claimSize,
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
	payload, err := decodeTokenReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	batch, err := w.reconcile(ctx, payload.environmentID, payload.tokenID, w.batchSize)
	if err != nil {
		return w.retry(ctx, message, err, outbox.RetryAfter(message.Attempts))
	}
	if batch.Deferred > 0 {
		return w.deferBatch(ctx, message, outbox.RetryAfter(message.Attempts))
	}
	if batch.Examined >= int(w.batchSize) {
		return w.continueBatch(ctx, message)
	}
	_, err = w.store.DeliverOutboxMessage(ctx, db.DeliverOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *DeliveryWorker) deferBatch(ctx context.Context, message db.OutboxMessage, after time.Duration) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(after)),
		LastError:    outbox.Error(errors.New("token reconciliation is waiting for checkpoint readiness"), "token reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *DeliveryWorker) continueBatch(ctx context.Context, message db.OutboxMessage) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC()),
		LastError:    outbox.Error(errors.New("token reconciliation batch continuation"), "token reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *DeliveryWorker) retry(ctx context.Context, message db.OutboxMessage, cause error, after time.Duration) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(after)),
		LastError:    outbox.Error(cause, "token reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

func (w *DeliveryWorker) deadLetter(ctx context.Context, message db.OutboxMessage, cause error) error {
	_, err := w.store.DeadLetterOutboxMessage(ctx, db.DeadLetterOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		LastError:    outbox.Error(cause, "token reconciliation delivery failed"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

type tokenReconcilePayload struct {
	EnvironmentID string `json:"environmentId"`
	TokenID       string `json:"tokenId"`
	environmentID uuid.UUID
	tokenID       uuid.UUID
}

func decodeTokenReconcilePayload(raw []byte) (tokenReconcilePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value tokenReconcilePayload
	if err := decoder.Decode(&value); err != nil {
		return tokenReconcilePayload{}, fmt.Errorf("decode token reconciliation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return tokenReconcilePayload{}, err
		}
		return tokenReconcilePayload{}, errors.New("token reconciliation payload contains a trailing JSON value")
	}
	environmentID, err := ids.Parse(value.EnvironmentID)
	if err != nil {
		return tokenReconcilePayload{}, errors.New("token reconciliation environmentId is invalid")
	}
	tokenID, err := ids.Parse(value.TokenID)
	if err != nil {
		return tokenReconcilePayload{}, errors.New("token reconciliation tokenId is invalid")
	}
	value.environmentID = environmentID
	value.tokenID = tokenID
	return value, nil
}
