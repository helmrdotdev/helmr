package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Go identifiers that encoding/json would otherwise place in client messages.
var goDecodeDiagnostics = []string{
	"Go struct", "Go value", "json:", "UnmarshalJSON", "CreateComputerRequest",
	"secretbinding", "Binding", "RunLeaseRenewRequest", "RunLeaseClaimRequest",
	"CompleteTaskRequest", "workerapi", "api.", "int64", "2006-01-02", "Manifest",
}

func assertNoGoDecodeDiagnostics(t *testing.T, message string) {
	t.Helper()
	for _, forbidden := range goDecodeDiagnostics {
		if strings.Contains(message, forbidden) {
			t.Fatalf("message %q contains %q", message, forbidden)
		}
	}
}

func decodeJSONBody(body string, out any) error {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return decodeRequestJSON(request, out)
}

func TestDecodeRequestJSONPublicMessages(t *testing.T) {
	tests := []struct {
		name string
		body string
		out  func() any
		want string
	}{
		{
			name: "mistyped nested field",
			body: `{"secrets":[{"secret":7}]}`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `field "secrets.0.secret" must be a JSON string, got number`,
		},
		{
			name: "mistyped array field",
			body: `{"secrets":{}}`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `field "secrets" must be a JSON array, got object`,
		},
		{
			name: "mistyped integer field",
			body: `{"lease_id":"x","lease_sequence":"1"}`,
			out:  func() any { return new(workerapi.RunLeaseClaimRequest) },
			want: `field "lease_sequence" must be a JSON integer, got string`,
		},
		{
			name: "fractional integer field",
			body: `{"lease_sequence":1.5}`,
			out:  func() any { return new(workerapi.RunLeaseClaimRequest) },
			want: `field "lease_sequence" must be an integer`,
		},
		{
			name: "overflowing integer field",
			body: `{"lease_sequence":99999999999999999999}`,
			out:  func() any { return new(workerapi.RunLeaseClaimRequest) },
			want: `field "lease_sequence" must be an integer within the supported range`,
		},
		{
			name: "invalid base64 field",
			body: `{"content":"not base64!"}`,
			out: func() any {
				return new(struct {
					Content []byte `json:"content"`
				})
			},
			want: `field "content" must be a base64-encoded string`,
		},
		{
			name: "base64 field with wrong type",
			body: `{"content":1}`,
			out: func() any {
				return new(struct {
					Content []byte `json:"content"`
				})
			},
			want: `field "content" must be a JSON string, got number`,
		},
		{
			name: "boolean for object",
			body: `true`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `value must be a JSON object, got boolean`,
		},
		{
			name: "unknown field",
			body: `{"secrets":[],"surprise":1}`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `unknown field "surprise"`,
		},
		{
			name: "syntax",
			body: `{"secrets":}`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `malformed JSON`,
		},
		{
			name: "truncated",
			body: `{"secrets":[`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `malformed JSON: unexpected end of input`,
		},
		{
			name: "empty",
			body: ``,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `request body is required`,
		},
		{
			name: "trailing",
			body: `{} {}`,
			out:  func() any { return new(api.CreateComputerRequest) },
			want: `request body must contain a single JSON value`,
		},
		{
			name: "timestamp type",
			body: `{"expected_expires_at":1}`,
			out:  func() any { return new(workerapi.RunLeaseRenewRequest) },
			want: `field "expected_expires_at" must be a JSON string, got number`,
		},
		{
			name: "timestamp format",
			body: `{"expected_expires_at":"tomorrow"}`,
			out:  func() any { return new(workerapi.RunLeaseRenewRequest) },
			want: `timestamp must be an RFC 3339 string`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeJSONBody(tt.body, tt.out())
			if err == nil || err.Error() != tt.want {
				t.Fatalf("decodeRequestJSON() error = %v, want %q", err, tt.want)
			}
			assertNoGoDecodeDiagnostics(t, err.Error())
		})
	}
}

func TestDecodeRequestJSONTimestampMessages(t *testing.T) {
	type optionalTimestamp struct {
		At *time.Time `json:"at"`
	}
	for _, value := range []string{`1`, `true`, `[]`, `{}`} {
		t.Run(value, func(t *testing.T) {
			var received string
			switch value[0] {
			case '1':
				received = "number"
			case 't':
				received = "boolean"
			case '[':
				received = "array"
			default:
				received = "object"
			}
			err := decodeJSONBody(`{"at":`+value+`}`, new(optionalTimestamp))
			want := `field "at" must be a JSON string, got ` + received
			if err == nil || err.Error() != want {
				t.Fatalf("decodeRequestJSON() error = %v, want %q", err, want)
			}
			assertNoGoDecodeDiagnostics(t, err.Error())
		})
	}
}

// Worker lease renewal decodes inline, so it exercises the handler-owned path.
func TestWorkerRenewRunLeaseTimestampMessages(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{`1`, `field "expected_expires_at" must be a JSON string, got number`},
		{`true`, `field "expected_expires_at" must be a JSON string, got boolean`},
		{`[]`, `field "expected_expires_at" must be a JSON string, got array`},
		{`{}`, `field "expected_expires_at" must be a JSON string, got object`},
		{`"tomorrow"`, `timestamp must be an RFC 3339 string`},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"expected_expires_at":`+tt.value+`}`))
			recorder := httptest.NewRecorder()
			(&Server{}).workerRenewRunLease(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
			var response api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			want := "invalid worker run lease renewal JSON: " + tt.want
			if response.Error.Code != "bad_request" || response.Error.Message != want {
				t.Fatalf("error = %+v, want bad_request %q", response.Error, want)
			}
			assertNoGoDecodeDiagnostics(t, response.Error.Message)
		})
	}
}

func TestCanonicalDecodersReportTruncatedBodies(t *testing.T) {
	bodies := []string{
		`{"computer":{"id":"x"},"payload":`,
		`{"computer":{"id":"x"},"payload":{"a":1}`,
		`{"idempotency_key":"k","data":[1,`,
		`{"computer":{"id":"x"},"payload":"abc`,
	}
	decoders := map[string]func(*http.Request) error{
		"task start": func(r *http.Request) error {
			_, _, err := decodeStartTaskRequest(r)
			return err
		},
		"actor start": func(r *http.Request) error {
			_, err := decodeStartActorRequest(r)
			return err
		},
		"session command": func(r *http.Request) error {
			return decodeSessionCommand(r, new(map[string]json.RawMessage))
		},
	}
	for name, decode := range decoders {
		for _, body := range bodies {
			t.Run(name+" "+body, func(t *testing.T) {
				err := decode(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
				if err == nil || !strings.HasSuffix(err.Error(), "malformed JSON: unexpected end of input") {
					t.Fatalf("decode error = %v, want truncated input", err)
				}
				assertNoGoDecodeDiagnostics(t, err.Error())
			})
		}
	}
}

func TestDecodeRequestJSONClassifiesFailures(t *testing.T) {
	oversized := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"secrets":[]}`))
	oversized.Body = http.MaxBytesReader(httptest.NewRecorder(), oversized.Body, 4)
	err := decodeRequestJSON(oversized, new(api.CreateComputerRequest))
	if !isRequestBodyTooLarge(err) || errorStatus(err) != http.StatusRequestEntityTooLarge || err.Error() != "request body is too large" {
		t.Fatalf("oversized error = %v status = %d", err, errorStatus(err))
	}
	err = decodeJSONBody(`{"secrets":7}`, new(api.CreateComputerRequest))
	if errorStatus(err) != http.StatusBadRequest {
		t.Fatalf("mistyped status = %d", errorStatus(err))
	}
	if err := decodeOptionalRequestJSON(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")), new(api.DeleteComputerRequest)); err != nil {
		t.Fatalf("optional empty body error = %v", err)
	}
}

// Oversized bodies are 413 even where a handler adds context to the error.
func TestWorkerDecodeReturnsTooLarge(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"lease":{"id":"`+strings.Repeat("x", 64)+`"}}`))
	recorder := httptest.NewRecorder()
	request.Body = http.MaxBytesReader(recorder, request.Body, 16)
	(&Server{}).workerRenewRunLease(recorder, request)
	body := decodeHTTPError(t, recorder.Body.Bytes())
	if recorder.Code != http.StatusRequestEntityTooLarge || body.Code != "request_too_large" ||
		body.Message != "invalid worker run lease renewal JSON: request body is too large" {
		t.Fatalf("status = %d error = %+v", recorder.Code, body)
	}
}

func TestNestedWorkerDecodeKeepsContext(t *testing.T) {
	var request workerapi.CompleteTaskRequest
	err := decodeClosedJSON(json.RawMessage(`{"surprise":true}`), &request)
	if err == nil || err.Error() != `decode task completion request: unknown field "surprise"` {
		t.Fatalf("decodeClosedJSON() error = %v", err)
	}
	assertNoGoDecodeDiagnostics(t, err.Error())
}

func TestParseRequestBodyDescribesDeploymentBundleFailures(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"contract":1}`))
	_, _, err := parseRequestBody(request, bundle.Parse)
	want := `decode deployment bundle: field "contract" must be a JSON string, got number`
	if err == nil || errorStatus(err) != http.StatusBadRequest || err.Error() != want {
		t.Fatalf("error = %v status = %d, want 400 %q", err, errorStatus(err), want)
	}
	assertNoGoDecodeDiagnostics(t, err.Error())

	oversized := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"contract":"x"}`))
	recorder := httptest.NewRecorder()
	limitRequestBodySize(recorder, oversized, 4)
	_, _, err = parseRequestBody(oversized, bundle.Parse)
	if errorStatus(err) != http.StatusRequestEntityTooLarge || err.Error() != "request body is too large" {
		t.Fatalf("oversized error = %v status = %d", err, errorStatus(err))
	}
}
