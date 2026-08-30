package secret

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestSecretRevocationDeliveryReconcilesAndDelivers(t *testing.T) {
	environmentID := uuid.NewV7()
	secretID := uuid.NewV7()
	message := secretRevocationMessage(environmentID, secretID, 3)
	store := &secretRevocationDeliveryStore{
		messages: []db.OutboxMessage{message},
	}
	var gotEnvironmentID, gotSecretID uuid.UUID
	var gotGeneration int64
	var gotLimit int32
	worker, err := NewRevocationDeliveryWorker(
		nil,
		store,
		func(
			_ context.Context,
			environmentID uuid.UUID,
			secretID uuid.UUID,
			generation int64,
			limit int32,
		) (int, error) {
			gotEnvironmentID = environmentID
			gotSecretID = secretID
			gotGeneration = generation
			gotLimit = limit
			return 1, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claim.Lane != "control" || len(store.claim.Topics) != 1 ||
		store.claim.Topics[0] != "secret.revoked" {
		t.Fatalf("claim = %+v", store.claim)
	}
	if gotEnvironmentID != environmentID || gotSecretID != secretID ||
		gotGeneration != 3 || gotLimit != secretRevocationBatchLimit {
		t.Fatalf(
			"reconcile authority = %s/%s/%d/%d",
			gotEnvironmentID,
			gotSecretID,
			gotGeneration,
			gotLimit,
		)
	}
	if store.delivered != message.Attempts || store.retried ||
		store.deadLettered {
		t.Fatalf(
			"delivery = delivered:%d retried:%v dead-lettered:%v",
			store.delivered,
			store.retried,
			store.deadLettered,
		)
	}
}

func TestSecretRevocationDeliveryContinuesFullBatch(t *testing.T) {
	message := secretRevocationMessage(
		uuid.NewV7(),
		uuid.NewV7(),
		1,
	)
	store := &secretRevocationDeliveryStore{
		messages: []db.OutboxMessage{message},
	}
	worker, err := NewRevocationDeliveryWorker(
		nil,
		store,
		func(
			context.Context,
			uuid.UUID,
			uuid.UUID,
			int64,
			int32,
		) (int, error) {
			return int(secretRevocationBatchLimit), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.retried || !store.retryAt.Equal(now) ||
		store.delivered != 0 || store.deadLettered {
		t.Fatalf(
			"continuation = retried:%v at:%s delivered:%d dead-lettered:%v",
			store.retried,
			store.retryAt,
			store.delivered,
			store.deadLettered,
		)
	}
}

func TestSecretRevocationDeliveryRetriesReconciliationFailure(t *testing.T) {
	message := secretRevocationMessage(
		uuid.NewV7(),
		uuid.NewV7(),
		1,
	)
	message.Attempts = 3
	store := &secretRevocationDeliveryStore{
		messages: []db.OutboxMessage{message},
	}
	worker, err := NewRevocationDeliveryWorker(
		nil,
		store,
		func(
			context.Context,
			uuid.UUID,
			uuid.UUID,
			int64,
			int32,
		) (int, error) {
			return 0, errors.New("database unavailable")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected reconciliation failure")
	}
	if !store.retried || !store.retryAt.Equal(now.Add(4*time.Second)) ||
		store.deadLettered {
		t.Fatalf(
			"retry = retried:%v at:%s dead-lettered:%v",
			store.retried,
			store.retryAt,
			store.deadLettered,
		)
	}
}

func TestSecretRevocationDeliveryDeadLettersInvalidPayload(t *testing.T) {
	message := secretRevocationMessage(
		uuid.NewV7(),
		uuid.NewV7(),
		1,
	)
	message.Payload = []byte(
		`{"environmentId":"invalid","secretId":"invalid","revocationGeneration":0}`,
	)
	store := &secretRevocationDeliveryStore{
		messages: []db.OutboxMessage{message},
	}
	worker, err := NewRevocationDeliveryWorker(
		nil,
		store,
		func(
			context.Context,
			uuid.UUID,
			uuid.UUID,
			int64,
			int32,
		) (int, error) {
			t.Fatal("invalid payload reached reconciler")
			return 0, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected invalid payload failure")
	}
	if !store.deadLettered || store.retried || store.delivered != 0 {
		t.Fatalf(
			"invalid delivery = dead-lettered:%v retried:%v delivered:%d",
			store.deadLettered,
			store.retried,
			store.delivered,
		)
	}
}

type secretRevocationDeliveryStore struct {
	messages     []db.OutboxMessage
	claim        db.ClaimOutboxMessagesParams
	delivered    int32
	retried      bool
	retryAt      time.Time
	deadLettered bool
}

func (s *secretRevocationDeliveryStore) ClaimOutboxMessages(
	_ context.Context,
	claim db.ClaimOutboxMessagesParams,
) ([]db.OutboxMessage, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *secretRevocationDeliveryStore) DeliverOutboxMessage(
	_ context.Context,
	params db.DeliverOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.delivered = params.ClaimAttempt
	return db.OutboxMessage{}, nil
}

func (s *secretRevocationDeliveryStore) RetryOutboxMessage(
	_ context.Context,
	params db.RetryOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.retried = true
	s.retryAt = params.AvailableAt.Time
	return db.OutboxMessage{}, nil
}

func (s *secretRevocationDeliveryStore) DeadLetterOutboxMessage(
	context.Context,
	db.DeadLetterOutboxMessageParams,
) (db.OutboxMessage, error) {
	s.deadLettered = true
	return db.OutboxMessage{}, nil
}

func secretRevocationMessage(
	environmentID uuid.UUID,
	secretID uuid.UUID,
	generation int64,
) db.OutboxMessage {
	return db.OutboxMessage{
		ID:    pgvalue.UUID(uuid.NewV7()),
		Lane:  "control",
		Topic: "secret.revoked",
		Payload: []byte(
			`{"environmentId":"` + environmentID.String() +
				`","secretId":"` + secretID.String() +
				`","revocationGeneration":` +
				fmt.Sprintf("%d", generation) + `}`,
		),
		State:          "claimed",
		Attempts:       1,
		ClaimedBy:      pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}
