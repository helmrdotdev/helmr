package runadmission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestActorInputDeliveryReconcilesTimeoutsAndIntent(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	actorID := uuid.Must(uuid.NewV7())
	recordID := uuid.Must(uuid.NewV7())
	message := actorInputReconcileMessage(environmentID, actorID, recordID)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	var gotEnvironmentID, gotActorID, gotRecordID uuid.UUID
	var timeoutLimit int32
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(_ context.Context, environmentID, actorID, recordID uuid.UUID) (bool, error) {
			gotEnvironmentID, gotActorID, gotRecordID = environmentID, actorID, recordID
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(_ context.Context, limit int32) (int, error) {
			timeoutLimit = limit
			return 1, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if timeoutLimit != tokenDeliveryClaimLimit || store.claim.Lane != "control" ||
		len(store.claim.Topics) != 2 ||
		store.claim.Topics[0] != "actor.input.reconcile" ||
		store.claim.Topics[1] != "actor.lifecycle.reconcile" {
		t.Fatalf("claim = %+v timeout limit = %d", store.claim, timeoutLimit)
	}
	if gotEnvironmentID != environmentID || gotActorID != actorID || gotRecordID != recordID {
		t.Fatalf("reconcile IDs = %s/%s/%s", gotEnvironmentID, gotActorID, gotRecordID)
	}
	if store.delivered != message.Attempts || store.retried || store.deadLettered {
		t.Fatalf("delivery state = delivered %d retried %v dead-lettered %v", store.delivered, store.retried, store.deadLettered)
	}
}

func TestActorInputDeliveryRetriesDeferredContinuation(t *testing.T) {
	message := actorInputReconcileMessage(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { return true, nil },
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, int32) (int, error) { return 0, nil },
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

func TestActorInputDeliveryReconcilesLifecycleIntent(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	actorID := uuid.Must(uuid.NewV7())
	message := actorLifecycleReconcileMessage(environmentID, actorID)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	var gotEnvironmentID, gotActorID uuid.UUID
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("lifecycle intent reached input reconciler")
			return false, nil
		},
		func(_ context.Context, environmentID, actorID uuid.UUID) (bool, error) {
			gotEnvironmentID, gotActorID = environmentID, actorID
			return false, nil
		},
		func(context.Context, int32) (int, error) { return 0, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotEnvironmentID != environmentID || gotActorID != actorID ||
		store.delivered != message.Attempts || store.retried || store.deadLettered {
		t.Fatalf(
			"lifecycle IDs=%s/%s delivery=%d retry=%v dead-letter=%v",
			gotEnvironmentID, gotActorID, store.delivered, store.retried, store.deadLettered,
		)
	}
}

func TestActorInputDeliveryRetriesDeferredLifecycle(t *testing.T) {
	message := actorLifecycleReconcileMessage(
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
	)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil },
		func(context.Context, int32) (int, error) { return 0, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("deferred lifecycle should report a retry cause")
	}
	if !store.retried || !store.retryAt.Equal(now.Add(time.Second)) ||
		store.delivered != 0 || store.deadLettered {
		t.Fatalf(
			"retry=%v at=%s delivered=%d dead-lettered=%v",
			store.retried, store.retryAt, store.delivered, store.deadLettered,
		)
	}
}

func TestActorInputDeliveryDeadLettersInvalidIntent(t *testing.T) {
	message := actorInputReconcileMessage(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	message.Payload = []byte(`{"environmentId":"invalid","actorId":"invalid","recordId":"invalid","extra":true}`)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
			t.Fatal("invalid intent reached reconciler")
			return false, nil
		},
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, int32) (int, error) { return 0, nil },
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

func TestActorInputDeliveryStopsBeforeClaimWhenTimeoutRepairFails(t *testing.T) {
	store := &tokenDeliveryStore{}
	worker, err := NewActorInputDeliveryWorker(nil, store,
		func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
		func(context.Context, int32) (int, error) { return 0, errors.New("database unavailable") },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("timeout reconciliation failure was ignored")
	}
	if store.claim.Lane != "" {
		t.Fatalf("outbox claimed after timeout failure: %+v", store.claim)
	}
}

func actorInputReconcileMessage(environmentID, actorID, recordID uuid.UUID) db.OutboxMessage {
	return db.OutboxMessage{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "actor.input.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","actorId":"` + actorID.String() + `","recordId":"` + recordID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}

func actorLifecycleReconcileMessage(environmentID, actorID uuid.UUID) db.OutboxMessage {
	return db.OutboxMessage{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "actor.lifecycle.reconcile",
		Payload: []byte(`{"environmentId":"` + environmentID.String() + `","actorId":"` + actorID.String() + `"}`),
		State:   "claimed", Attempts: 1, ClaimedBy: pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}
