package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func newTestEventStream(t *testing.T) *EventStream {
	t.Helper()
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	return &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}
}

func TestTaskStartClaimUsesOwnerTokenForRelease(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	ctx := context.Background()

	claim, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt()); !errors.Is(err, errTaskStartPending) {
		t.Fatalf("second claim err = %v, want pending", err)
	}
	claim.release(ctx)
	if _, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt()); err != nil {
		t.Fatalf("claim after release error = %v", err)
	}
}

func TestTaskStartClaimSerializesIdempotencyKeyAcrossRequestFingerprints(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	ctx := context.Background()

	if _, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt()); !errors.Is(err, errTaskStartPending) {
		t.Fatalf("different fingerprint claim err = %v, want pending", err)
	}
}

func TestTaskStartClaimSkipsMissingIdempotencyKey(t *testing.T) {
	server := &Server{}
	claim, err := server.claimTaskStart(context.Background(), dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "", pgtype.Timestamptz{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.active || claim.resolved || len(claim.keys) != 0 {
		t.Fatalf("claim = %+v, want no Redis claim without idempotency key", claim)
	}
}

func TestTaskStartClaimReturnsResolvedHintForDurableReread(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	ctx := context.Background()

	claim, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if err != nil {
		t.Fatal(err)
	}
	claim.resolve(ctx)
	retry, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if err != nil {
		t.Fatalf("claim after stale resolved hint error = %v", err)
	}
	if retry.active || !retry.resolved {
		t.Fatalf("retry claim = %+v, want inactive resolved hint", retry)
	}
}

func TestTaskStartClaimResolvedHintCoversAllIdentityKeys(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	ctx := context.Background()

	claim, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if err != nil {
		t.Fatal(err)
	}
	claim.resolve(ctx)
	retry, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if err != nil {
		t.Fatal(err)
	}
	if !retry.resolved || len(retry.keys) != 1 {
		t.Fatalf("retry claim = %+v, want resolved hint for idempotency key", retry)
	}
	retry.clearResolved(ctx)
	key := taskStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idempotency", "idem")
	exists, err := redisClient.Exists(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("idempotency key still exists after clearResolved")
	}
}

func TestTaskStartClaimRedisFailureReturnsCoordinationUnavailable(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	redisServer.Close()

	_, err := server.claimTaskStart(context.Background(), dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", taskStartClaimTestExpiresAt())
	if !errors.Is(err, errTaskStartCoordinationUnavailable) {
		t.Fatalf("claim error = %v, want coordination unavailable", err)
	}
}

func TestTaskStartClaimKeyIncludesScope(t *testing.T) {
	key := taskStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "external", "ext")
	if want := pgvalue.MustUUIDValue(testProjectID()).String(); !containsAll(key, dbtest.DefaultOrgID.String(), want, pgvalue.MustUUIDValue(testEnvironmentID()).String()) {
		t.Fatalf("claim key %q does not include org/project/environment scope", key)
	}
}

func TestTaskStartClaimCapsTTLToIdempotencyExpiry(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := &Server{eventStream: &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}}
	ctx := context.Background()
	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(time.Second), Valid: true}

	claim, err := server.claimTaskStart(ctx, dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idem", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	key := taskStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idempotency", "idem")
	pendingTTL, err := redisClient.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pendingTTL <= 0 || pendingTTL > time.Second {
		t.Fatalf("pending TTL = %s, want capped to idempotency expiry", pendingTTL)
	}
	claim.resolve(ctx)
	resolvedTTL, err := redisClient.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if resolvedTTL <= 0 || resolvedTTL > time.Second {
		t.Fatalf("resolved TTL = %s, want capped to idempotency expiry", resolvedTTL)
	}
}

func taskStartClaimTestExpiresAt() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
