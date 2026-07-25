package executor

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func TestWorkerRunMetadataRequestPreservesClosedMutation(t *testing.T) {
	amount := 2.5
	request, err := workerRunMetadataRequest(&runv0.MetadataUpdated{
		CorrelationId: "00000000-0000-0000-0000-000000000001",
		Operation:     "increment",
		Key:           stringPointer("steps"),
		Amount:        &amount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.OperationID != "00000000-0000-0000-0000-000000000001" ||
		request.Operation != "increment" ||
		request.Key != "steps" ||
		request.Amount == nil ||
		*request.Amount != amount {
		t.Fatalf("request = %+v", request)
	}
}

func TestWorkerStructuredLogRequestUsesObservedSequence(t *testing.T) {
	request, err := workerStructuredLogRequest(
		&runv0.StructuredLogRequested{
			CorrelationId:  "00000000-0000-0000-0000-000000000001",
			Level:          "warn",
			Message:        "retrying",
			AttributesJson: `{"attempt":2}`,
		},
		9,
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ObservedSeq != 9 ||
		request.Level != "warn" ||
		request.Message != "retrying" ||
		string(request.Attributes) != `{"attempt":2}` {
		t.Fatalf("request = %+v", request)
	}
}

func TestRuntimeOperationFailureKeepsSemanticControlError(t *testing.T) {
	failure, ok := runtimeOperationFailure(
		&client.HTTPError{
			StatusCode: http.StatusUnprocessableEntity,
			Code:       "run_metadata_rejected",
			Message:    "metadata is too large",
		},
		"fallback",
		"fallback",
	)
	if !ok {
		t.Fatal("semantic HTTP error was not recognized")
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"code":"run_metadata_rejected","message":"metadata is too large","retryable":false}` {
		t.Fatalf("failure = %s", raw)
	}
}
