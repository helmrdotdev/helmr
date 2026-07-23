package control

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/publicid"
)

func TestNormalizeActorStartCanonicalizesAnnotationsAndPreservesInputPresence(t *testing.T) {
	now := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	key := "thread:42"
	ttl := maxQueuedRunTTLMS
	normalized, err := normalizeActorStart(actorStartRequest{
		OrgID: uuid.Must(uuid.NewV7()), ProjectID: uuid.Must(uuid.NewV7()),
		EnvironmentID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()),
		ActorDeclaredID: "operator.v1", WorkspaceAddress: json.RawMessage(`{"id":"wsp_example"}`),
		Key: &key, InputPresent: true, Input: json.RawMessage(`null`),
		Metadata: json.RawMessage(`{"b":2,"a":1}`), Tags: []string{" beta ", "alpha", "alpha"},
		ManagedQueueName: "default", ManagedQueuedTTLMS: &ttl,
		ManagedRetryPolicy: json.RawMessage(`{"enabled":false}`),
		ManagedRunMetadata: json.RawMessage(`{"request":1}`), ManagedRunTags: []string{"run"},
		ExpiresAt: timePointer(now.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.Metadata) != `{"a":1,"b":2}` {
		t.Fatalf("metadata = %s", normalized.Metadata)
	}
	if len(normalized.Tags) != 2 || normalized.Tags[0] != "alpha" || normalized.Tags[1] != "beta" {
		t.Fatalf("tags = %#v", normalized.Tags)
	}
	if !normalized.InputPresent || string(normalized.Input) != "null" {
		t.Fatalf("input present=%v value=%s", normalized.InputPresent, normalized.Input)
	}
	if normalized.ManagedQueuedTTLMS == nil || *normalized.ManagedQueuedTTLMS != maxQueuedRunTTLMS {
		t.Fatalf("queued TTL = %v", normalized.ManagedQueuedTTLMS)
	}
}

func TestNormalizeActorStartRejectsInvalidCallerOverridesAndOversizeFields(t *testing.T) {
	base := actorStartRequest{
		OrgID: uuid.Must(uuid.NewV7()), ProjectID: uuid.Must(uuid.NewV7()),
		EnvironmentID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()),
		ActorDeclaredID: "operator.v1", WorkspaceAddress: json.RawMessage(`{"id":"wsp_example"}`),
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
	oversize.Tags = []string{string(make([]byte, maxActorTagBytes+1))}
	if _, err := normalizeActorStart(oversize); !errors.Is(err, errActorStartInvalid) {
		t.Fatalf("oversize tag error = %v", err)
	}
}

func TestNormalizeActorStartUsesExactConcurrencyKeyBoundaryDomain(t *testing.T) {
	base := actorStartRequest{
		OrgID: uuid.Must(uuid.NewV7()), ProjectID: uuid.Must(uuid.NewV7()),
		EnvironmentID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()),
		ActorDeclaredID: "operator.v1", WorkspaceAddress: json.RawMessage(`{"id":"wsp_example"}`),
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
	actorPublicID, err := publicid.New(publicid.Actor)
	if err != nil {
		t.Fatal(err)
	}
	runPublicID, err := publicid.New(publicid.Run)
	if err != nil {
		t.Fatal(err)
	}
	recordID := uuid.Must(uuid.NewV7())
	value := actorStartResult{
		ActorID: uuid.Must(uuid.NewV7()), ActorPublicID: actorPublicID,
		InitialRecordID: &recordID, BootRunID: uuid.Must(uuid.NewV7()), BootRunPublicID: runPublicID,
	}
	raw, err := json.Marshal(actorStartReceiptFromResult(value))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := actorStartResultFromReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ActorID != value.ActorID || decoded.ActorPublicID != value.ActorPublicID ||
		decoded.InitialRecordID == nil || *decoded.InitialRecordID != recordID ||
		decoded.BootRunID != value.BootRunID || decoded.BootRunPublicID != value.BootRunPublicID {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
