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

func TestDeliveryWorkerReconcilesAndDeliversTokenIntent(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	tokenID := uuid.Must(uuid.NewV7())
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	var gotEnvironmentID, gotTokenID uuid.UUID
	var gotLimit int32
	worker, err := NewTokenDeliveryWorker(nil, store, func(
		_ context.Context,
		environmentID uuid.UUID,
		tokenID uuid.UUID,
		limit int32,
	) (db.TokenWaitReconcileBatch, error) {
		gotEnvironmentID, gotTokenID, gotLimit = environmentID, tokenID, limit
		return db.TokenWaitReconcileBatch{Examined: 1, Resolved: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claim.Lane != "control" || len(store.claim.Topics) != 1 ||
		store.claim.Topics[0] != "token.reconcile" {
		t.Fatalf("claim = %+v", store.claim)
	}
	if store.expireLimit != tokenReconcileBatchLimit {
		t.Fatalf("Token expiry limit = %d", store.expireLimit)
	}
	if store.credentialExpireLimit != tokenReconcileBatchLimit {
		t.Fatalf("Token credential expiry limit = %d", store.credentialExpireLimit)
	}
	if gotEnvironmentID != environmentID || gotTokenID != tokenID || gotLimit != tokenReconcileBatchLimit {
		t.Fatalf("reconcile IDs/limit = %s/%s/%d", gotEnvironmentID, gotTokenID, gotLimit)
	}
	if store.delivered != message.Attempts || store.retried || store.deadLettered {
		t.Fatalf("delivery state = delivered %d retried %v dead-lettered %v", store.delivered, store.retried, store.deadLettered)
	}
}

func TestDeliveryWorkerContinuesFullBoundedBatch(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	tokenID := uuid.Must(uuid.NewV7())
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewTokenDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (db.TokenWaitReconcileBatch, error) {
		return db.TokenWaitReconcileBatch{Examined: int(tokenReconcileBatchLimit), Resolved: int(tokenReconcileBatchLimit)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.retried || !store.retryAt.Equal(now) || store.delivered != 0 || store.deadLettered {
		t.Fatalf("continuation = retried %v at %s delivered %d dead-lettered %v", store.retried, store.retryAt, store.delivered, store.deadLettered)
	}
}

func TestDeliveryWorkerRetriesReconciliationFailure(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	tokenID := uuid.Must(uuid.NewV7())
	message := tokenReconcileMessage(environmentID, tokenID)
	message.Attempts = 3
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewTokenDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (db.TokenWaitReconcileBatch, error) {
		return db.TokenWaitReconcileBatch{}, errors.New("database unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected reconciliation failure")
	}
	if !store.retried || !store.retryAt.Equal(now.Add(4*time.Second)) || store.deadLettered {
		t.Fatalf("retry = retried %v at %s dead-lettered %v", store.retried, store.retryAt, store.deadLettered)
	}
}

func TestDeliveryWorkerRetainsIntentWhileCheckpointReadinessIsPending(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	tokenID := uuid.Must(uuid.NewV7())
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewTokenDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (db.TokenWaitReconcileBatch, error) {
		return db.TokenWaitReconcileBatch{Examined: 1, Deferred: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.retried || !store.retryAt.Equal(now.Add(time.Second)) || store.delivered != 0 || store.deadLettered {
		t.Fatalf("deferred = retried %v at %s delivered %d dead-lettered %v", store.retried, store.retryAt, store.delivered, store.deadLettered)
	}
}

func TestDeliveryWorkerDeadLettersInvalidTokenIntent(t *testing.T) {
	message := tokenReconcileMessage(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	message.Payload = []byte(`{"environmentId":"invalid","tokenId":"invalid"}`)
	store := &tokenDeliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewTokenDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (db.TokenWaitReconcileBatch, error) {
		t.Fatal("invalid payload reached reconciler")
		return db.TokenWaitReconcileBatch{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected invalid payload failure")
	}
	if !store.deadLettered || store.retried || store.delivered != 0 {
		t.Fatalf("invalid delivery = dead-lettered %v retried %v delivered %d", store.deadLettered, store.retried, store.delivered)
	}
}

type tokenDeliveryStore struct {
	messages              []db.OutboxMessage
	claim                 db.ClaimOutboxMessagesParams
	delivered             int32
	retried               bool
	retryAt               time.Time
	deadLettered          bool
	expireLimit           int32
	credentialExpireLimit int32
}

func (s *tokenDeliveryStore) ExpireDueTokens(_ context.Context, params db.ExpireDueTokensParams) ([]db.ExpireDueTokensRow, error) {
	s.expireLimit = params.LimitCount
	return nil, nil
}

func (s *tokenDeliveryStore) ExpireDuePublicAccessTokens(_ context.Context, limit int32) ([]db.PublicAccessToken, error) {
	s.credentialExpireLimit = limit
	return nil, nil
}

func (s *tokenDeliveryStore) ClaimOutboxMessages(_ context.Context, claim db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *tokenDeliveryStore) DeliverOutboxMessage(_ context.Context, params db.DeliverOutboxMessageParams) (db.OutboxMessage, error) {
	s.delivered = params.ClaimAttempt
	return db.OutboxMessage{}, nil
}

func (s *tokenDeliveryStore) RetryOutboxMessage(_ context.Context, params db.RetryOutboxMessageParams) (db.OutboxMessage, error) {
	s.retried = true
	s.retryAt = params.AvailableAt.Time
	return db.OutboxMessage{}, nil
}

func (s *tokenDeliveryStore) DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error) {
	s.deadLettered = true
	return db.OutboxMessage{}, nil
}

func tokenReconcileMessage(environmentID, tokenID uuid.UUID) db.OutboxMessage {
	return db.OutboxMessage{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Lane:           "control",
		Topic:          "token.reconcile",
		Payload:        []byte(`{"environmentId":"` + environmentID.String() + `","tokenId":"` + tokenID.String() + `"}`),
		State:          "claimed",
		Attempts:       1,
		ClaimedBy:      pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}
