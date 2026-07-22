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
	deliveryPollInterval = 250 * time.Millisecond
	deliveryClaimLease   = 30 * time.Second
	deliveryClaimLimit   = int32(100)
)

type DeliveryStore interface {
	ClaimOutboxMessages(context.Context, db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error)
	GetRun(context.Context, db.GetRunParams) (db.Run, error)
	DeliverOutboxMessage(context.Context, db.DeliverOutboxMessageParams) (db.OutboxMessage, error)
	RetryOutboxMessage(context.Context, db.RetryOutboxMessageParams) (db.OutboxMessage, error)
	DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error)
}

type RunEnqueue func(context.Context, pgtype.UUID, pgtype.UUID) error
type RunResumeEnqueue func(context.Context, pgtype.UUID, pgtype.UUID, pgtype.UUID, int64) error

type DeliveryWorker struct {
	log       *slog.Logger
	store     DeliveryStore
	enqueue   RunEnqueue
	resume    RunResumeEnqueue
	workerID  string
	interval  time.Duration
	claimFor  time.Duration
	claimSize int32
	now       func() time.Time
}

func NewDeliveryWorker(
	log *slog.Logger,
	store DeliveryStore,
	enqueue RunEnqueue,
	resume RunResumeEnqueue,
) (*DeliveryWorker, error) {
	if store == nil {
		return nil, errors.New("Run admission delivery store is required")
	}
	if enqueue == nil {
		return nil, errors.New("Run admission enqueuer is required")
	}
	if resume == nil {
		return nil, errors.New("Run resume enqueuer is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeliveryWorker{
		log:       log,
		store:     store,
		enqueue:   enqueue,
		resume:    resume,
		workerID:  uuid.Must(uuid.NewV7()).String(),
		interval:  deliveryPollInterval,
		claimFor:  deliveryClaimLease,
		claimSize: deliveryClaimLimit,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("Run admission delivery failed", "error", err)
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
		Lane:           "control",
		Topics:         []string{"run.admit", "run.resume"},
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
	switch message.Topic {
	case "run.admit":
		return w.processAdmission(ctx, message)
	case "run.resume":
		return w.processResume(ctx, message)
	default:
		return w.deadLetter(ctx, message, errors.New("Run delivery topic is unsupported"))
	}
}

func (w *DeliveryWorker) processAdmission(ctx context.Context, message db.OutboxMessage) error {
	payload, err := decodeRunAdmissionPayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	run, err := w.store.GetRun(ctx, db.GetRunParams{
		EnvironmentID: pgvalue.UUID(payload.environmentID),
		ID:            pgvalue.UUID(payload.runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return w.deadLetter(ctx, message, errors.New("Run admission authority does not exist"))
	}
	if err != nil {
		return w.retry(ctx, message, err)
	}
	if err := w.enqueue(ctx, run.OrgID, run.ID); err != nil {
		return w.retry(ctx, message, err)
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

func (w *DeliveryWorker) processResume(ctx context.Context, message db.OutboxMessage) error {
	payload, err := decodeRunResumePayload(message.Payload)
	if err != nil {
		return w.deadLetter(ctx, message, err)
	}
	if err := w.resume(
		ctx,
		pgvalue.UUID(payload.environmentID),
		pgvalue.UUID(payload.runID),
		pgvalue.UUID(payload.runWaitID),
		payload.ResumeRequestVersion,
	); err != nil {
		return w.retry(ctx, message, err)
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

func (w *DeliveryWorker) retry(ctx context.Context, message db.OutboxMessage, cause error) error {
	_, err := w.store.RetryOutboxMessage(ctx, db.RetryOutboxMessageParams{
		ID:           message.ID,
		ClaimedBy:    message.ClaimedBy,
		ClaimAttempt: message.Attempts,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(deliveryRetryAfter(message.Attempts))),
		LastError:    deliveryError(cause),
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
		LastError:    deliveryError(cause),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return errors.Join(cause, err)
}

type runAdmissionPayload struct {
	EnvironmentID string `json:"environmentId"`
	RunID         string `json:"runId"`
	environmentID uuid.UUID
	runID         uuid.UUID
}

type runResumePayload struct {
	EnvironmentID        string `json:"environmentId"`
	RunID                string `json:"runId"`
	RunWaitID            string `json:"runWaitId"`
	ResumeRequestVersion int64  `json:"resumeRequestVersion"`
	environmentID        uuid.UUID
	runID                uuid.UUID
	runWaitID            uuid.UUID
}

func decodeRunAdmissionPayload(raw []byte) (runAdmissionPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload runAdmissionPayload
	if err := decoder.Decode(&payload); err != nil {
		return runAdmissionPayload{}, fmt.Errorf("decode Run admission payload: %w", err)
	}
	if err := ensureDeliveryEOF(decoder); err != nil {
		return runAdmissionPayload{}, err
	}
	environmentID, err := uuid.Parse(payload.EnvironmentID)
	if err != nil {
		return runAdmissionPayload{}, errors.New("Run admission environmentId is invalid")
	}
	runID, err := uuid.Parse(payload.RunID)
	if err != nil {
		return runAdmissionPayload{}, errors.New("Run admission runId is invalid")
	}
	payload.environmentID = environmentID
	payload.runID = runID
	return payload, nil
}

func decodeRunResumePayload(raw []byte) (runResumePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload runResumePayload
	if err := decoder.Decode(&payload); err != nil {
		return runResumePayload{}, fmt.Errorf("decode Run resume payload: %w", err)
	}
	if err := ensureDeliveryEOF(decoder); err != nil {
		return runResumePayload{}, err
	}
	environmentID, err := uuid.Parse(payload.EnvironmentID)
	if err != nil {
		return runResumePayload{}, errors.New("Run resume environmentId is invalid")
	}
	runID, err := uuid.Parse(payload.RunID)
	if err != nil {
		return runResumePayload{}, errors.New("Run resume runId is invalid")
	}
	runWaitID, err := uuid.Parse(payload.RunWaitID)
	if err != nil {
		return runResumePayload{}, errors.New("Run resume runWaitId is invalid")
	}
	if payload.ResumeRequestVersion <= 0 {
		return runResumePayload{}, errors.New("Run resume resumeRequestVersion must be positive")
	}
	payload.environmentID = environmentID
	payload.runID = runID
	payload.runWaitID = runWaitID
	return payload, nil
}

func ensureDeliveryEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("Run admission payload contains a trailing JSON value")
}

func deliveryRetryAfter(attempt int32) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	if attempt >= 7 {
		return time.Minute
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func deliveryError(err error) []byte {
	message := []rune(err.Error())
	if len(message) > 2048 {
		message = message[:2048]
	}
	raw, marshalErr := json.Marshal(map[string]string{"message": string(message)})
	if marshalErr != nil {
		return []byte(`{"message":"Run admission delivery failed"}`)
	}
	return raw
}
