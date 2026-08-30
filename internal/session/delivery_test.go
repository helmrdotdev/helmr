package session

import (
	"context"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestSessionInputDeliveryReconcilesIntent(t *testing.T) {
	environmentID := uuid.NewV7()
	sessionID := uuid.NewV7()
	recordID := uuid.NewV7()
	message := sessionInputReconcileMessage(environmentID, sessionID, recordID)
	store := &sessionDeliveryStore{messages: []db.OutboxMessage{message}}
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
	if store.claim.Lane != "control" ||
		len(store.claim.Topics) != 2 ||
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
	store := &sessionDeliveryStore{messages: []db.OutboxMessage{message}}
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
	store := &sessionDeliveryStore{messages: []db.OutboxMessage{message}}
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
	store := &sessionDeliveryStore{messages: []db.OutboxMessage{message}}
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
	store := &sessionDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewDeliveryWorker(nil, store,
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
}

func sessionInputReconcileMessage(environmentID, sessionID, recordID uuid.UUID) db.OutboxMessage {
	return db.OutboxMessage{
		ID: pgvalue.UUID(uuid.NewV7()), Lane: "control", Topic: "session.input.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","sessionId":"` + sessionID.String() + `","recordId":"` + recordID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}

func sessionCloseReconcileMessage(environmentID, sessionID uuid.UUID) db.OutboxMessage {
	return db.OutboxMessage{
		ID: pgvalue.UUID(uuid.NewV7()), Lane: "control", Topic: "session.close.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","sessionId":"` + sessionID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}

type sessionDeliveryStore struct {
	messages     []db.OutboxMessage
	claim        db.ClaimOutboxMessagesParams
	delivered    int32
	retried      bool
	retryAt      time.Time
	deadLettered bool
}

func (s *sessionDeliveryStore) ClaimOutboxMessages(
	_ context.Context,
	claim db.ClaimOutboxMessagesParams,
) ([]db.OutboxMessage, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *sessionDeliveryStore) DeliverOutboxMessage(
	_ context.Context,
	params db.DeliverOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.delivered = params.ClaimAttempt
	return db.OutboxMessage{}, nil
}

func (s *sessionDeliveryStore) RetryOutboxMessage(
	_ context.Context,
	params db.RetryOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.retried = true
	s.retryAt = params.AvailableAt.Time
	return db.OutboxMessage{}, nil
}

func (s *sessionDeliveryStore) DeadLetterOutboxMessage(
	context.Context,
	db.DeadLetterOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.deadLettered = true
	return db.OutboxMessage{}, nil
}
