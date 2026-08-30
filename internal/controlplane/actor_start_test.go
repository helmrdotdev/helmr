package controlplane

import (
	"encoding/json"
	"errors"
	"testing"

	"uuid"
)

func TestNormalizeActorStartCanonicalizesRunOptionsAndPreservesInputPresence(t *testing.T) {
	key := "thread:42"
	ttl := maxQueuedRunTTLMS
	workspaceID := uuid.NewV7()
	normalized, err := normalizeActorStart(actorStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID:   uuid.NewV7(),
		ActorDeclaredID: "operator.v1", WorkspaceID: workspaceID,
		Key: &key, InputPresent: true, Input: json.RawMessage(`null`),
		ManagedQueueName: "default", ManagedQueuedTTLMS: &ttl,
		ManagedRetryPolicy: json.RawMessage(`{"enabled":false}`),
		ManagedRunMetadata: json.RawMessage(`{"b":2,"a":1}`),
		ManagedRunTags:     []string{" beta ", "alpha", "alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.ManagedRunMetadata) != `{"a":1,"b":2}` {
		t.Fatalf("managed Run metadata = %s", normalized.ManagedRunMetadata)
	}
	if len(normalized.ManagedRunTags) != 2 ||
		normalized.ManagedRunTags[0] != "alpha" ||
		normalized.ManagedRunTags[1] != "beta" {
		t.Fatalf("managed Run tags = %#v", normalized.ManagedRunTags)
	}
	if !normalized.InputPresent || string(normalized.Input) != "null" {
		t.Fatalf("input present=%v value=%s", normalized.InputPresent, normalized.Input)
	}
	if normalized.ManagedQueuedTTLMS == nil || *normalized.ManagedQueuedTTLMS != maxQueuedRunTTLMS {
		t.Fatalf("queued TTL = %v", normalized.ManagedQueuedTTLMS)
	}
}

func TestNormalizeActorStartLimitsNormalizedTagSet(t *testing.T) {
	workspaceID := uuid.NewV7()
	request := actorStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID:   uuid.NewV7(),
		ActorDeclaredID: "operator.v1", WorkspaceID: workspaceID,
		ManagedRunTags: []string{"same", "same", "same", "same", "same", "same", "same", "same", "same", "same", "same"},
	}
	normalized, err := normalizeActorStart(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.ManagedRunTags) != 1 || normalized.ManagedRunTags[0] != "same" {
		t.Fatalf("managed Run tags = %#v", normalized.ManagedRunTags)
	}
}

func TestNormalizeActorStartRejectsInvalidCallerOverridesAndOversizeFields(t *testing.T) {
	workspaceID := uuid.NewV7()
	base := actorStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID:   uuid.NewV7(),
		ActorDeclaredID: "operator.v1", WorkspaceID: workspaceID,
		ManagedQueueName: "default",
	}
	tooLongTTL := maxQueuedRunTTLMS + 1
	invalidTTL := base
	invalidTTL.ManagedQueuedTTLMS = &tooLongTTL
	if _, err := normalizeActorStart(invalidTTL); !errors.Is(err, errActorStartInvalid) {
		t.Fatalf("queued TTL error = %v", err)
	}
	invalidRetry := base
	invalidRetry.ManagedRetryPolicy = json.RawMessage(`{"enabled":true,"maxAttempts":3}`)
	if _, err := normalizeActorStart(invalidRetry); !errors.Is(err, errActorStartInvalid) {
		t.Fatalf("retry error = %v", err)
	}
	oversize := base
	oversize.ManagedRunTags = []string{string(make([]byte, maxTagBytes+1))}
	if _, err := normalizeActorStart(oversize); !errors.Is(err, errActorStartInvalid) {
		t.Fatalf("oversize tag error = %v", err)
	}
}

func TestNormalizeActorStartUsesExactConcurrencyKeyBoundaryDomain(t *testing.T) {
	workspaceID := uuid.NewV7()
	base := actorStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID:   uuid.NewV7(),
		ActorDeclaredID: "operator.v1", WorkspaceID: workspaceID,
	}
	nonBreakingSpace := "\u00a0opaque\u00a0"
	base.ManagedConcurrencyKey = &nonBreakingSpace
	if _, err := normalizeActorStart(base); err != nil {
		t.Fatalf("non-ASCII edge space is opaque: %v", err)
	}
	for _, value := range []string{" leading", "trailing\t", "nul\x00byte"} {
		request := base
		request.ManagedConcurrencyKey = &value
		if _, err := normalizeActorStart(request); !errors.Is(err, errActorStartInvalid) {
			t.Fatalf("concurrency key %q error = %v", value, err)
		}
	}
}

func TestActorStartReceiptRoundTrip(t *testing.T) {
	recordID := uuid.NewV7()
	value := actorStartResult{
		SessionID:       uuid.NewV7(),
		InitialRecordID: &recordID, BootRunID: uuid.NewV7(),
	}
	raw, err := json.Marshal(actorStartReceiptFromResult(value))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := actorStartResultFromReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != value.SessionID ||
		decoded.InitialRecordID == nil || *decoded.InitialRecordID != recordID ||
		decoded.BootRunID != value.BootRunID {
		t.Fatalf("decoded = %+v", decoded)
	}
}
