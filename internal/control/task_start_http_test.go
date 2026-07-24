package control

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
		strings.NewReader(`{"payload":null,"options":{"workspace":{"key":"workspace:1"}}}`),
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
		`{"options":{"workspace":null}}`,
		`{"options":{"workspace":{"key":null}}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"unknown":true}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"idempotency_key":null}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"idempotency_key":""}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"queue":""}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"ttl":""}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"metadata":null}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"tags":[null]}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"retry":{"enabled":null}}}`,
		`{"options":{"workspace":{"key":"workspace:1"},"retry":{"backoff":{"factor":null}}}}`,
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
		{err: idempotency.ConflictError{}, status: http.StatusConflict, code: "idempotency_conflict"},
		{err: errTaskNotDeployed, status: http.StatusNotFound, code: "task_not_deployed"},
		{err: errTaskWorkspaceNotFound, status: http.StatusNotFound, code: "workspace_not_found"},
		{err: errTaskWorkspaceUnavailable, status: http.StatusConflict, code: "workspace_unavailable"},
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
