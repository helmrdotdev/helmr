package controlplane

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/idempotency"
)

func TestDecodeStartTaskRequestIsClosedAndPayloadPresenceAware(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"payload":null,"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}}`),
	)
	decoded, payloadPresent, err := decodeStartTaskRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !payloadPresent || string(decoded.Payload) != "null" {
		t.Fatalf("payload present=%v value=%s", payloadPresent, decoded.Payload)
	}

	for _, body := range []string{
		`null`,
		`{}`,
		`{"options":null}`,
		`{"computer":null}`,
		`{"computer":{"key":"computer:1"}}`,
		`{"computer":{"key":null}}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"unknown":true}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":null}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"queue":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"ttl":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"metadata":null}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"tags":[null]}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"retry":{"enabled":null}}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"retry":{"backoff":{"factor":null}}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, _, err := decodeStartTaskRequest(request); err == nil {
			t.Fatalf("decodeStartTaskRequest(%s) succeeded", body)
		}
	}
}

func TestWriteTaskStartErrorUsesStableCodes(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{err: conflict(codedError{code: "computer_preparation_exhausted", message: "Computer preparation limit reached"}), status: http.StatusConflict, code: "computer_preparation_exhausted"},
		{err: idempotency.ConflictError{}, status: http.StatusConflict, code: "idempotency_conflict"},
		{err: errTaskNotDeployed, status: http.StatusNotFound, code: "task_not_deployed"},
		{err: errTaskComputerNotFound, status: http.StatusNotFound, code: "computer_not_found"},
		{err: errTaskComputerUnavailable, status: http.StatusConflict, code: "computer_unavailable"},
		{err: errTaskSecretUnavailable, status: http.StatusConflict, code: "secret_unavailable"},
		{err: errTaskPayloadPresenceInvalid, status: http.StatusBadRequest, code: "invalid_task_start"},
		{err: errors.New("database failed"), status: http.StatusServiceUnavailable, code: "task_start_authority_unavailable"},
	} {
		recorder := httptest.NewRecorder()
		server.writeTaskStartError(recorder, test.err)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("error %v response = %d %s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}
