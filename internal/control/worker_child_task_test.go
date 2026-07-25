package control

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
)

func TestNormalizeWorkerChildTaskRequestUsesParentScopeAndCallerOptions(t *testing.T) {
	workspaceID := actorStartTestPublicID(t, publicid.Workspace)
	concurrencyKey := "customer:1"
	normalized, err := normalizeWorkerChildTaskRequest(
		api.WorkerInvokeChildTaskRequest{
			TaskDeclaredID: "resize-image",
			PayloadPresent: true,
			Payload:        json.RawMessage(`{"b":2,"a":1}`),
			Workspace:      json.RawMessage(`{"id":"` + workspaceID + `"}`),
			Options: json.RawMessage(`{
				"queue":"images",
				"concurrency_key":"customer:1",
				"priority":10,
				"ttl":"1m",
				"retry":{
					"max_attempts":3,
					"backoff":{"min_delay":"1s","max_delay":"30s","factor":2,"jitter":"full"}
				},
				"metadata":{"source":"parent"},
				"tags":[" resize ","image","image"]
			}`),
			IdempotencyKey: "resize:image-1",
		},
		db.GetLiveRunLeaseLocatorsRow{
			OrgID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.Payload) != `{"a":1,"b":2}` ||
		normalized.QueueName != "images" ||
		normalized.ConcurrencyKey == nil ||
		*normalized.ConcurrencyKey != concurrencyKey ||
		normalized.Priority != 10 ||
		normalized.QueuedTTLMS == nil ||
		*normalized.QueuedTTLMS != 60_000 ||
		string(normalized.RetryPolicy) != `{"backoff":{"factor":2,"jitter":"full","maxMs":30000,"minMs":1000},"enabled":true,"maxAttempts":3}` ||
		normalized.IdempotencyKey != "resize:image-1" {
		t.Fatalf("normalized = %+v", normalized)
	}
	if len(normalized.Tags) != 2 ||
		normalized.Tags[0] != "image" ||
		normalized.Tags[1] != "resize" {
		t.Fatalf("tags = %#v", normalized.Tags)
	}
}

func TestChildWorkspacePairLockIsOrderIndependent(t *testing.T) {
	left := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	right := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	if childWorkspacePairLock(left, right) != childWorkspacePairLock(right, left) {
		t.Fatal("Workspace pair lock depends on argument order")
	}
	if childWorkspacePairLock(left, right) == childWorkspacePairLock(left, left) {
		t.Fatal("distinct Workspace pairs produced the same lock key")
	}
}

func TestDecodeChildTaskReceiptRequiresCanonicalAuthority(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	runPublicID := actorStartTestPublicID(t, publicid.Run)
	receipt, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","runPublicId":"` + runPublicID +
			`","workspaceId":"` + workspaceID.String() + `"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID != runID.String() ||
		receipt.RunPublicID != runPublicID ||
		receipt.WorkspaceID != workspaceID.String() {
		t.Fatalf("receipt = %+v", receipt)
	}
	if _, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","runPublicId":"` + runPublicID +
			`","workspaceId":"00000000-0000-0000-0000-000000000000"}`,
	)); !errors.Is(err, errTaskStartReceiptInvalid) {
		t.Fatalf("nil Workspace receipt error = %v", err)
	}
}
