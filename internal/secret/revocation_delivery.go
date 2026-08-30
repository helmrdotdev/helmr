package secret

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
	secretRevocationPollInterval = 250 * time.Millisecond
	secretRevocationClaimLease   = 30 * time.Second
	secretRevocationClaimLimit   = int32(32)
	secretRevocationBatchLimit   = int32(100)
)

type RevocationDeliveryStore interface {
	ClaimOutboxMessages(
		context.Context,
		db.ClaimOutboxMessagesParams,
	) ([]db.OutboxMessage, error)
	DeliverOutboxMessage(
		context.Context,
		db.DeliverOutboxMessageParams,
	) (db.OutboxMessage, error)
	RetryOutboxMessage(
		context.Context,
		db.RetryOutboxMessageParams,
	) (db.OutboxMessage, error)
	DeadLetterOutboxMessage(
		context.Context,
		db.DeadLetterOutboxMessageParams,
	) (db.OutboxMessage, error)
}

type RevocationReconcileBatch func(
	context.Context,
	uuid.UUID,
	uuid.UUID,
	int64,
	int32,
) (int, error)

type RevocationDeliveryWorker struct {
	log       *slog.Logger
	store     RevocationDeliveryStore
	reconcile RevocationReconcileBatch
	workerID  string
	interval  time.Duration
	claimFor  time.Duration
	claimSize int32
	batchSize int32
	now       func() time.Time
}

func NewRevocationDeliveryWorker(
	log *slog.Logger,
	store RevocationDeliveryStore,
	reconcile RevocationReconcileBatch,
) (*RevocationDeliveryWorker, error) {
	if store == nil {
		return nil, errors.New("secret revocation delivery store is required")
	}
	if reconcile == nil {
		return nil, errors.New("secret revocation reconciler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RevocationDeliveryWorker{
		log:       log,
		store:     store,
		reconcile: reconcile,
		workerID:  uuid.NewV7().String(),
		interval:  secretRevocationPollInterval,
		claimFor:  secretRevocationClaimLease,
		claimSize: secretRevocationClaimLimit,
		batchSize: secretRevocationBatchLimit,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *RevocationDeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Secret revocation delivery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *RevocationDeliveryWorker) tick(ctx context.Context) error {
	now := w.now().UTC()
	messages, err := w.store.ClaimOutboxMessages(
		ctx,
		db.ClaimOutboxMessagesParams{
			ClaimedBy:      pgtype.Text{String: w.workerID, Valid: true},
			ClaimExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(now.Add(w.claimFor)),
			Lane:           "control",
			Topics:         []string{"secret.revoked"},
			RowLimit:       w.claimSize,
		},
	)
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

func (w *RevocationDeliveryWorker) process(
	ctx context.Context,
	message db.OutboxMessage,
) error {
	payload, err := decodeSecretRevocationPayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	examined, err := w.reconcile(
		ctx,
		payload.environmentID,
		payload.secretID,
		payload.RevocationGeneration,
		w.batchSize,
	)
	if err != nil {
		return w.retry(
			ctx,
			message,
			err,
			outbox.RetryAfter(message.Attempts),
		)
	}
	if examined >= int(w.batchSize) {
		return w.continueBatch(ctx, message)
	}
	_, err = w.store.DeliverOutboxMessage(
		ctx,
		db.DeliverOutboxMessageParams{
			ID:           message.ID,
			ClaimedBy:    message.ClaimedBy,
			ClaimAttempt: message.Attempts,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *RevocationDeliveryWorker) continueBatch(
	ctx context.Context,
	message db.OutboxMessage,
) error {
	_, err := w.store.RetryOutboxMessage(
		ctx,
		db.RetryOutboxMessageParams{
			ID:           message.ID,
			ClaimedBy:    message.ClaimedBy,
			ClaimAttempt: message.Attempts,
			AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC()),
			LastError: outbox.Error(
				errors.New("secret revocation batch continuation"),
				"secret revocation delivery failed",
			),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *RevocationDeliveryWorker) retry(
	ctx context.Context,
	message db.OutboxMessage,
	cause error,
	after time.Duration,
) error {
	_, err := w.store.RetryOutboxMessage(
		ctx,
		db.RetryOutboxMessageParams{
			ID:           message.ID,
			ClaimedBy:    message.ClaimedBy,
			ClaimAttempt: message.Attempts,
			AvailableAt: pgvalue.TimestamptzUTCZeroInvalid(
				w.now().UTC().Add(after),
			),
			LastError: outbox.Error(cause, "secret revocation delivery failed"),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

func (w *RevocationDeliveryWorker) deadLetter(
	ctx context.Context,
	message db.OutboxMessage,
	cause error,
) error {
	_, err := w.store.DeadLetterOutboxMessage(
		ctx,
		db.DeadLetterOutboxMessageParams{
			ID:           message.ID,
			ClaimedBy:    message.ClaimedBy,
			ClaimAttempt: message.Attempts,
			LastError:    outbox.Error(cause, "secret revocation delivery failed"),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

type secretRevocationPayload struct {
	EnvironmentID        string `json:"environmentId"`
	SecretID             string `json:"secretId"`
	RevocationGeneration int64  `json:"revocationGeneration"`
	environmentID        uuid.UUID
	secretID             uuid.UUID
}

func decodeSecretRevocationPayload(
	raw []byte,
) (secretRevocationPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value secretRevocationPayload
	if err := decoder.Decode(&value); err != nil {
		return secretRevocationPayload{},
			fmt.Errorf("decode secret revocation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return secretRevocationPayload{}, err
		}
		return secretRevocationPayload{},
			errors.New("secret revocation payload contains a trailing JSON value")
	}
	environmentID, err := ids.Parse(value.EnvironmentID)
	if err != nil {
		return secretRevocationPayload{},
			errors.New("secret revocation environmentId is invalid")
	}
	secretID, err := ids.Parse(value.SecretID)
	if err != nil {
		return secretRevocationPayload{},
			errors.New("secret revocation secretId is invalid")
	}
	if value.RevocationGeneration <= 0 {
		return secretRevocationPayload{},
			errors.New("secret revocation generation must be positive")
	}
	value.environmentID = environmentID
	value.secretID = secretID
	return value, nil
}
