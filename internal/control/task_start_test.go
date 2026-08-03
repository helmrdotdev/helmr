package control

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
)

func TestNormalizeTaskStartCanonicalizesCallerSemantics(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7()).String()
	ttl := int64(60_000)
	concurrencyKey := "customer:1"
	normalized, err := normalizeTaskStart(taskStartRequest{
		OrgID: uuid.Must(uuid.NewV7()), ProjectID: uuid.Must(uuid.NewV7()),
		EnvironmentID: uuid.Must(uuid.NewV7()), TaskDeclaredID: "resize-image",
		PayloadPresent: true, Payload: json.RawMessage(`{"b":2,"a":1}`),
		Workspace: api.WorkspaceTarget{ID: &workspaceID},
		QueueName: "images", ConcurrencyKey: &concurrencyKey, QueuedTTLMS: &ttl,
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
	workspaceID := uuid.Must(uuid.NewV7()).String()
	base := taskStartRequest{
		OrgID: uuid.Must(uuid.NewV7()), ProjectID: uuid.Must(uuid.NewV7()),
		EnvironmentID: uuid.Must(uuid.NewV7()), TaskDeclaredID: "task",
		Workspace: api.WorkspaceTarget{ID: &workspaceID},
	}
	invalidKey := " leading"
	request := base
	request.ConcurrencyKey = &invalidKey
	if _, err := normalizeTaskStart(request); !errors.Is(err, errTaskStartInvalid) {
		t.Fatalf("concurrency key error = %v", err)
	}
	request = base
	request.PayloadPresent = true
	request.Payload = json.RawMessage(`{"broken"`)
	if _, err := normalizeTaskStart(request); !errors.Is(err, errTaskStartInvalid) {
		t.Fatalf("payload error = %v", err)
	}
	request = base
	request.Tags = make([]string, maxTags+1)
	for index := range request.Tags {
		request.Tags[index] = string(rune('a' + index))
	}
	if _, err := normalizeTaskStart(request); !errors.Is(err, errTaskStartInvalid) {
		t.Fatalf("tags error = %v", err)
	}
}

func TestTaskStartReceiptRoundTrip(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	raw, err := json.Marshal(taskStartReceipt{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskStartResultFromReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RunID != runID {
		t.Fatalf("decoded = %+v", decoded)
	}
}
