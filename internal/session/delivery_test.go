package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestSessionInputDeliveryReconcilesIntent(t *testing.T) {
	environmentID := uuid.NewV7()
	sessionID := uuid.NewV7()
	recordID := uuid.NewV7()
	message := sessionInputReconcileMessage(environmentID, sessionID, recordID)
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	var gotEnvironmentID, gotSessionID, gotRecordID uuid.UUID
	worker, err := NewDeliveryWorker(nil, store,
		func(_ context.Context, environmentID, sessionID, recordID uuid.UUID) (bool, error) {
			gotEnvironmentID, gotSessionID, gotRecordID = environmentID, sessionID, recordID
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.claim.Topics) != 2 ||
		store.claim.Topics[0] != "session.input.reconcile" ||
		store.claim.Topics[1] != "session.close.reconcile" {
		t.Fatalf("claim = %+v", store.claim)
	}
	if gotEnvironmentID != environmentID || gotSessionID != sessionID || gotRecordID != recordID {
		t.Fatalf("reconcile IDs = %s/%s/%s", gotEnvironmentID, gotSessionID, gotRecordID)
	}
	if store.delivered != message.Attempts || store.retried || store.deadLettered {
		t.Fatalf("delivery state = delivered %d retried %v dead-lettered %v", store.delivered, store.retried, store.deadLettered)
	}
}

func TestSessionInputDeliveryRetriesDeferredContinuation(t *testing.T) {
	message := sessionInputReconcileMessage(uuid.NewV7(), uuid.NewV7(), uuid.NewV7())
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	worker, err := NewDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { return true, nil },
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("deferred continuation should report a retry cause")
	}
	if !store.retried || !store.retryAt.Equal(now.Add(time.Second)) || store.delivered != 0 || store.deadLettered {
		t.Fatalf("retry = retried %v at %s delivered %d dead-lettered %v", store.retried, store.retryAt, store.delivered, store.deadLettered)
	}
}

func TestSessionInputDeliveryReconcilesCloseIntent(t *testing.T) {
	environmentID := uuid.NewV7()
	sessionID := uuid.NewV7()
	message := sessionCloseReconcileMessage(environmentID, sessionID)
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	var gotEnvironmentID, gotSessionID uuid.UUID
	worker, err := NewDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("close intent reached input reconciler")
			return false, nil
		},
		func(_ context.Context, environmentID, sessionID uuid.UUID) (bool, error) {
			gotEnvironmentID, gotSessionID = environmentID, sessionID
			return false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotEnvironmentID != environmentID || gotSessionID != sessionID ||
		store.delivered != message.Attempts || store.retried || store.deadLettered {
		t.Fatalf(
			"close IDs=%s/%s delivery=%d retry=%v dead-letter=%v",
			gotEnvironmentID, gotSessionID, store.delivered, store.retried, store.deadLettered,
		)
	}
}

func TestSessionInputDeliveryRetriesDeferredClose(t *testing.T) {
	message := sessionCloseReconcileMessage(
		uuid.NewV7(),
		uuid.NewV7(),
	)
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	worker, err := NewDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("deferred close should report a retry cause")
	}
	if !store.retried || !store.retryAt.Equal(now.Add(time.Second)) ||
		store.delivered != 0 || store.deadLettered {
		t.Fatalf(
			"retry=%v at=%s delivered=%d dead-lettered=%v",
			store.retried, store.retryAt, store.delivered, store.deadLettered,
		)
	}
}

func TestSessionInputDeliveryDeadLettersInvalidIntent(t *testing.T) {
	message := sessionInputReconcileMessage(uuid.NewV7(), uuid.NewV7(), uuid.NewV7())
	message.Payload = []byte(`{"environmentId":"invalid","sessionId":"invalid","recordId":"invalid","extra":true}`)
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	var logs bytes.Buffer
	worker, err := NewDeliveryWorker(
		slog.New(slog.NewJSONHandler(&logs, nil)),
		store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("invalid intent reached reconciler")
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("invalid payload should surface its dead-letter cause")
	}
	if !store.deadLettered || store.retried || store.delivered != 0 {
		t.Fatalf("invalid delivery = dead-lettered %v retried %v delivered %d", store.deadLettered, store.retried, store.delivered)
	}
	if !strings.Contains(logs.String(), `"msg":"control outbox dead-lettered"`) ||
		!strings.Contains(logs.String(), `"topic":"session.input.reconcile"`) ||
		!strings.Contains(logs.String(), pgvalue.UUIDString(message.ID)) {
		t.Fatalf("dead-letter warning = %s", logs.String())
	}
}

func TestSessionInputDeliveryDeadLettersUnsupportedTopic(t *testing.T) {
	message := sessionInputReconcileMessage(uuid.NewV7(), uuid.NewV7(), uuid.NewV7())
	message.Topic = "session.unknown.reconcile"
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}}
	var logs bytes.Buffer
	worker, err := NewDeliveryWorker(
		slog.New(slog.NewJSONHandler(&logs, nil)),
		store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("unsupported topic reached input reconciler")
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("unsupported topic reached close reconciler")
			return false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("unsupported topic should surface its dead-letter cause")
	}
	if !store.deadLettered || !strings.Contains(logs.String(), `"topic":"session.unknown.reconcile"`) {
		t.Fatalf("unsupported topic = dead-lettered %v logs %s", store.deadLettered, logs.String())
	}
}

func TestSessionInputDeliveryDoesNotWarnWhenDeadLetterClaimIsLost(t *testing.T) {
	message := sessionInputReconcileMessage(uuid.NewV7(), uuid.NewV7(), uuid.NewV7())
	message.Payload = []byte(`{"environmentId":"invalid","sessionId":"invalid","recordId":"invalid"}`)
	store := &sessionDeliveryStore{messages: []db.ControlOutbox{message}, deadLetterErr: pgx.ErrNoRows}
	var logs bytes.Buffer
	worker, err := NewDeliveryWorker(
		slog.New(slog.NewJSONHandler(&logs, nil)),
		store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("invalid intent reached reconciler")
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatalf("lost claim = %v", err)
	}
	if store.deadLettered || strings.Contains(logs.String(), "control outbox dead-lettered") {
		t.Fatalf("lost claim warned = dead-lettered %v logs %s", store.deadLettered, logs.String())
	}
}

func sessionInputReconcileMessage(environmentID, sessionID, recordID uuid.UUID) db.ControlOutbox {
	return db.ControlOutbox{
		ID:      pgvalue.UUID(uuid.NewV7()),
		Topic:   "session.input.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","sessionId":"` + sessionID.String() + `","recordId":"` + recordID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}

func sessionCloseReconcileMessage(environmentID, sessionID uuid.UUID) db.ControlOutbox {
	return db.ControlOutbox{
		ID:      pgvalue.UUID(uuid.NewV7()),
		Topic:   "session.close.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","sessionId":"` + sessionID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}

type sessionDeliveryStore struct {
	messages      []db.ControlOutbox
	claim         db.ClaimControlOutboxParams
	delivered     int32
	retried       bool
	retryAt       time.Time
	deadLettered  bool
	deadLetterErr error
}

func (s *sessionDeliveryStore) ClaimControlOutbox(
	_ context.Context,
	claim db.ClaimControlOutboxParams,
) ([]db.ControlOutbox, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *sessionDeliveryStore) DeliverControlOutbox(
	_ context.Context,
	params db.DeliverControlOutboxParams,
) (db.ControlOutbox, error) {
	s.delivered = params.ClaimAttempt
	return db.ControlOutbox{}, nil
}

func (s *sessionDeliveryStore) RetryControlOutbox(
	_ context.Context,
	params db.RetryControlOutboxParams,
) (db.ControlOutbox, error) {
	s.retried = true
	s.retryAt = params.AvailableAt.Time
	return db.ControlOutbox{}, nil
}

func (s *sessionDeliveryStore) DeadLetterControlOutbox(
	context.Context,
	db.DeadLetterControlOutboxParams,
) (db.ControlOutbox, error) {
	if s.deadLetterErr != nil {
		return db.ControlOutbox{}, s.deadLetterErr
	}
	s.deadLettered = true
	return db.ControlOutbox{}, nil
}
