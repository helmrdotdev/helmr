package token

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDeliveryWorkerReconcilesAndDeliversTokenIntent(t *testing.T) {
	environmentID := uuid.NewV7()
	tokenID := uuid.NewV7()
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.ControlOutbox{message}}
	var gotEnvironmentID, gotTokenID uuid.UUID
	var gotLimit int32
	worker, err := NewDeliveryWorker(nil, store, func(
		_ context.Context,
		environmentID uuid.UUID,
		tokenID uuid.UUID,
		limit int32,
	) (WaitBatch, error) {
		gotEnvironmentID, gotTokenID, gotLimit = environmentID, tokenID, limit
		return WaitBatch{Examined: 1, Resolved: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.claim.Topics) != 1 ||
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
	environmentID := uuid.NewV7()
	tokenID := uuid.NewV7()
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.ControlOutbox{message}}
	worker, err := NewDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (WaitBatch, error) {
		return WaitBatch{Examined: int(tokenReconcileBatchLimit), Resolved: int(tokenReconcileBatchLimit)}, nil
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
	environmentID := uuid.NewV7()
	tokenID := uuid.NewV7()
	message := tokenReconcileMessage(environmentID, tokenID)
	message.Attempts = 3
	store := &tokenDeliveryStore{messages: []db.ControlOutbox{message}}
	worker, err := NewDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (WaitBatch, error) {
		return WaitBatch{}, errors.New("database unavailable")
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
	environmentID := uuid.NewV7()
	tokenID := uuid.NewV7()
	message := tokenReconcileMessage(environmentID, tokenID)
	store := &tokenDeliveryStore{messages: []db.ControlOutbox{message}}
	worker, err := NewDeliveryWorker(nil, store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (WaitBatch, error) {
		return WaitBatch{Examined: 1, Deferred: 1}, nil
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
	message := tokenReconcileMessage(uuid.NewV7(), uuid.NewV7())
	message.Payload = []byte(`{"environmentId":"invalid","tokenId":"invalid"}`)
	store := &tokenDeliveryStore{messages: []db.ControlOutbox{message}}
	var logs bytes.Buffer
	worker, err := NewDeliveryWorker(slog.New(slog.NewJSONHandler(&logs, nil)), store, func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		int32,
	) (WaitBatch, error) {
		t.Fatal("invalid payload reached reconciler")
		return WaitBatch{}, nil
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
	if !strings.Contains(logs.String(), `"msg":"control outbox dead-lettered"`) ||
		!strings.Contains(logs.String(), `"topic":"token.reconcile"`) ||
		!strings.Contains(logs.String(), pgvalue.UUIDString(message.ID)) {
		t.Fatalf("dead-letter warning = %s", logs.String())
	}
}

type tokenDeliveryStore struct {
	messages              []db.ControlOutbox
	claim                 db.ClaimControlOutboxParams
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

func (s *tokenDeliveryStore) ClaimControlOutbox(_ context.Context, claim db.ClaimControlOutboxParams) ([]db.ControlOutbox, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *tokenDeliveryStore) DeliverControlOutbox(_ context.Context, params db.DeliverControlOutboxParams) (db.ControlOutbox, error) {
	s.delivered = params.ClaimAttempt
	return db.ControlOutbox{}, nil
}

func (s *tokenDeliveryStore) RetryControlOutbox(_ context.Context, params db.RetryControlOutboxParams) (db.ControlOutbox, error) {
	s.retried = true
	s.retryAt = params.AvailableAt.Time
	return db.ControlOutbox{}, nil
}

func (s *tokenDeliveryStore) DeadLetterControlOutbox(context.Context, db.DeadLetterControlOutboxParams) (db.ControlOutbox, error) {
	s.deadLettered = true
	return db.ControlOutbox{}, nil
}

func tokenReconcileMessage(environmentID, tokenID uuid.UUID) db.ControlOutbox {
	return db.ControlOutbox{
		ID:             pgvalue.UUID(uuid.NewV7()),
		Topic:          "token.reconcile",
		Payload:        []byte(`{"environmentId":"` + environmentID.String() + `","tokenId":"` + tokenID.String() + `"}`),
		State:          "claimed",
		Attempts:       1,
		ClaimedBy:      pgvalue.Text("worker"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}
}
