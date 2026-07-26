package runadmission

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

type TokenDeliveryStore interface {
	ExpireDueTokens(context.Context, db.ExpireDueTokensParams) ([]db.ExpireDueTokensRow, error)
	ExpireDuePublicAccessTokens(context.Context, int32) ([]db.PublicAccessToken, error)
	ClaimOutboxMessages(context.Context, db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error)
	DeliverOutboxMessage(context.Context, db.DeliverOutboxMessageParams) (db.OutboxMessage, error)
	RetryOutboxMessage(context.Context, db.RetryOutboxMessageParams) (db.OutboxMessage, error)
	DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error)
}

type TokenReconcileBatch func(context.Context, uuid.UUID, uuid.UUID, int32) (db.TokenWaitReconcileBatch, error)

type TokenDeliveryWorker struct {
	log       *slog.Logger
	store     TokenDeliveryStore
	reconcile TokenReconcileBatch
	workerID  string
	interval  time.Duration
	claimFor  time.Duration
	claimSize int32
	batchSize int32
	now       func() time.Time
}

func NewTokenDeliveryWorker(log *slog.Logger, store TokenDeliveryStore, reconcile TokenReconcileBatch) (*TokenDeliveryWorker, error) {
	if store == nil {
		return nil, errors.New("Token reconciliation delivery store is required")
	}
	if reconcile == nil {
		return nil, errors.New("Token Wait reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &TokenDeliveryWorker{
		log:       log,
		store:     store,
		reconcile: reconcile,
		workerID:  uuid.Must(uuid.NewV7()).String(),
		interval:  tokenDeliveryPollInterval,
		claimFor:  tokenDeliveryClaimLease,
		claimSize: tokenDeliveryClaimLimit,
		batchSize: tokenReconcileBatchLimit,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *TokenDeliveryWorker) Run(ctx context.Context) error {
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

func (w *TokenDeliveryWorker) tick(ctx context.Context) error {
	if _, err := w.store.ExpireDueTokens(ctx, db.ExpireDueTokensParams{
		OutboxMessageIds: pgvalue.NewUUIDv7Batch(w.batchSize),
		LimitCount:       w.batchSize,
	}); err != nil {
		return fmt.Errorf("expire due Tokens: %w", err)
	}
	if _, err := w.store.ExpireDuePublicAccessTokens(ctx, w.batchSize); err != nil {
		return fmt.Errorf("expire due Token public access credentials: %w", err)
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

func (w *TokenDeliveryWorker) process(ctx context.Context, message db.OutboxMessage) error {
	payload, err := decodeTokenReconcilePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	batch, err := w.reconcile(ctx, payload.environmentID, payload.tokenID, w.batchSize)
	if err != nil {
		return w.retry(ctx, message, err, tokenDeliveryRetryAfter(message.Attempts))
	}
	if batch.Deferred > 0 {
		return w.deferBatch(ctx, message, tokenDeliveryRetryAfter(message.Attempts))
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

func (w *TokenDeliveryWorker) deferBatch(ctx context.Context, message db.OutboxMessage, after time.Duration) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(after)),
		LastError:    tokenDeliveryError(errors.New("Token reconciliation is waiting for checkpoint readiness")),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *TokenDeliveryWorker) continueBatch(ctx context.Context, message db.OutboxMessage) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC()),
		LastError:    tokenDeliveryError(errors.New("Token reconciliation batch continuation")),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *TokenDeliveryWorker) retry(ctx context.Context, message db.OutboxMessage, cause error, after time.Duration) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(after)),
		LastError:    tokenDeliveryError(cause),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

func (w *TokenDeliveryWorker) deadLetter(ctx context.Context, message db.OutboxMessage, cause error) error {
	_, err := w.store.DeadLetterOutboxMessage(ctx, db.DeadLetterOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		LastError:    tokenDeliveryError(cause),
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
		return tokenReconcilePayload{}, fmt.Errorf("decode Token reconciliation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return tokenReconcilePayload{}, err
		}
		return tokenReconcilePayload{}, errors.New("Token reconciliation payload contains a trailing JSON value")
	}
	environmentID, err := uuid.Parse(value.EnvironmentID)
	if err != nil {
		return tokenReconcilePayload{}, errors.New("Token reconciliation environmentId is invalid")
	}
	tokenID, err := uuid.Parse(value.TokenID)
	if err != nil {
		return tokenReconcilePayload{}, errors.New("Token reconciliation tokenId is invalid")
	}
	value.environmentID = environmentID
	value.tokenID = tokenID
	return value, nil
}

func tokenDeliveryRetryAfter(attempt int32) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	if attempt >= 7 {
		return time.Minute
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func tokenDeliveryError(err error) []byte {
	message := []rune(err.Error())
	if len(message) > 2048 {
		message = message[:2048]
	}
	raw, marshalErr := json.Marshal(map[string]string{"message": string(message)})
	if marshalErr != nil {
		return []byte(`{"message":"Token reconciliation delivery failed"}`)
	}
	return raw
}
