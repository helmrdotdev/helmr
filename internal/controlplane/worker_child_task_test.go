package controlplane

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestChildTaskInvokeStaleResponseIncludesClosedFailurePoint(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
	response := httptest.NewRecorder()
	server.writeChildTaskInvokeError(
		response,
		"0198b960-7818-7a77-9d7d-4ebf163e15b1",
		run.ErrChildInvokeSourceScope,
	)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		`"code":"child_task_invoke_stale"`,
		`"details":{"point":"source_scope"}`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("response omitted %s: %s", want, response.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "failure_point=source_scope") {
		t.Fatalf("log omitted failure point: %s", logs.String())
	}
}

func TestChildRequestRejectsExplicitNullRetryPolicy(t *testing.T) {
	var request idempotency.TaskChildInvokeFingerprint
	if err := decodeClosedJSON(json.RawMessage(`{
		"method":"call",
		"payloadPresent":false,
		"computer":{},
		"queueName":"default",
		"priority":0,
		"retryPolicy":null,
		"metadata":{},
		"tags":[]
	}`), &request); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.ParseRetry(request.RetryPolicy); err == nil {
		t.Fatal("explicit null retry policy was accepted")
	}
}

func TestNormalizeWorkerChildTaskRequestUsesParentScopeAndCallerOptions(t *testing.T) {
	computerID := uuid.NewV7().String()
	concurrencyKey := "customer:1"
	normalized, err := normalizeWorkerChildTaskRequest(
		workerapi.InvokeChildTaskRequest{
			TaskDeclaredID: "resize-image",
			PayloadPresent: true,
			Payload:        json.RawMessage(`{"b":2,"a":1}`),
			Computer:       json.RawMessage(`{"id":"` + computerID + `"}`),
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
			OrgID:         pgvalue.UUID(uuid.NewV7()),
			ProjectID:     pgvalue.UUID(uuid.NewV7()),
			EnvironmentID: pgvalue.UUID(uuid.NewV7()),
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
