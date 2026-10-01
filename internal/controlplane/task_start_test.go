package controlplane

import (
	"encoding/json"
	"errors"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/run"
)

func TestNormalizeTaskStartCanonicalizesCallerSemantics(t *testing.T) {
	computerID := uuid.NewV7()
	ttl := int64(60_000)
	concurrencyKey := "customer:1"
	normalized, err := normalizeTaskStart(taskStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID: uuid.NewV7(), TaskDeclaredID: "resize-image",
		PayloadPresent: true, Payload: json.RawMessage(`{"b":2,"a":1}`),
		ComputerID: computerID,
		QueueName:  "images", ConcurrencyKey: &concurrencyKey, QueuedTTLMS: &ttl,
		Metadata: json.RawMessage(`{"source":"backend"}`),
		Tags:     []string{" resize ", "image", "image"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.Payload) != `{"a":1,"b":2}` {
		t.Fatalf("payload = %s", normalized.Payload)
	}
	if len(normalized.Tags) != 2 || normalized.Tags[0] != "image" || normalized.Tags[1] != "resize" {
		t.Fatalf("tags = %#v", normalized.Tags)
	}
	if !normalized.PayloadPresent || normalized.QueuedTTLMS == nil || *normalized.QueuedTTLMS != ttl {
		t.Fatalf("normalized = %+v", normalized)
	}
}

func TestNormalizeTaskStartRejectsInvalidCallerValues(t *testing.T) {
	computerID := uuid.NewV7()
	base := taskStartRequest{
		OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID: uuid.NewV7(), TaskDeclaredID: "task",
		ComputerID: computerID,
	}
	invalidKey := " leading"
	request := base
	request.ConcurrencyKey = &invalidKey
	if _, err := normalizeTaskStart(request); !errors.Is(err, run.ErrTaskStartInvalid) {
		t.Fatalf("concurrency key error = %v", err)
	}
	request = base
	request.PayloadPresent = true
	request.Payload = json.RawMessage(`{"broken"`)
	if _, err := normalizeTaskStart(request); !errors.Is(err, run.ErrTaskStartInvalid) {
		t.Fatalf("payload error = %v", err)
	}
	request = base
	request.Tags = make([]string, maxTags+1)
	for index := range request.Tags {
		request.Tags[index] = string(rune('a' + index))
	}
	if _, err := normalizeTaskStart(request); !errors.Is(err, run.ErrTaskStartInvalid) {
		t.Fatalf("tags error = %v", err)
	}
}
