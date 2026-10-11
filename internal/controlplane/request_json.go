package controlplane

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// This file owns reading and decoding client request bodies. Handlers read a
// body only through these helpers, so every decoding failure is classified
// (400, or 413 once the body limit is exceeded) and described in API terms:
// JSON field paths, JSON types and unknown member names, never server type or
// struct names. TestRequestBodiesDecodeThroughOwner enforces the boundary.

// limitRequestBody rejects request bodies larger than limit with 413.
func limitRequestBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, tooLarge(errors.New("request body is too large")))
				return
			}
			limitRequestBodySize(w, r, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// limitRequestBodySize lowers the body limit for one handler; reading past it
// fails as too large.
func limitRequestBodySize(w http.ResponseWriter, r *http.Request, limit int64) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
}

// decodeRequestJSON decodes a required request body holding exactly one JSON
// value and rejects members that out does not declare.
func decodeRequestJSON(r *http.Request, out any) error {
	return decodeRequestBody(r, out, false)
}

// decodeOptionalRequestJSON is decodeRequestJSON for bodies that may be empty.
func decodeOptionalRequestJSON(r *http.Request, out any) error {
	return decodeRequestBody(r, out, true)
}

func decodeRequestBody(r *http.Request, out any, optional bool) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			if optional {
				return nil
			}
			return badRequest(errors.New("request body is required"))
		}
		return requestBodyError(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if isRequestBodyTooLarge(err) {
			return requestBodyError(err)
		}
		return badRequest(errors.New("request body must contain a single JSON value"))
	}
	return nil
}

// readRequestBody reads a whole request body for decoders that must inspect
// its raw JSON before binding it.
func readRequestBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, requestBodyError(err)
	}
	return raw, nil
}

// parseRequestBody reads a whole request body and parses it with the parser of
// the package that owns its format, describing JSON failures in API terms.
// It returns the body the value was parsed from.
func parseRequestBody[T any](r *http.Request, parse func([]byte) (T, error)) (T, []byte, error) {
	var zero T
	raw, err := readRequestBody(r)
	if err != nil {
		return zero, nil, err
	}
	value, err := parse(raw)
	if err != nil {
		return zero, nil, badRequest(publicJSONDecodeError(err))
	}
	return value, raw, nil
}

// canonicalJSON canonicalizes one JSON value held in request-derived or stored
// bytes. Its errors are left for the caller to classify.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, publicJSONDecodeError(err)
	}
	return json.RawMessage(canonical), nil
}

func isRequestBodyTooLarge(err error) bool {
	var size *http.MaxBytesError
	return errors.As(err, &size)
}

func requestBodyError(err error) error {
	if isRequestBodyTooLarge(err) {
		return tooLarge(&jsonDecodeError{message: "request body is too large", cause: err})
	}
	return badRequest(publicJSONDecodeError(err))
}

// jsonDecodeError carries a decoding failure whose message is expressed in API
// terms. The cause stays reachable, for example for body size checks.
type jsonDecodeError struct {
	message string
	cause   error
}

func (e *jsonDecodeError) Error() string { return e.message }

func (e *jsonDecodeError) Unwrap() error { return e.cause }

const (
	jsonUnknownFieldPrefix   = "json: unknown field "
	jsonUnexpectedEndMessage = "unexpected end of JSON input"
	jsonTruncatedMessage     = "malformed JSON: unexpected end of input"
	// jsonBase64Type marks byte slices, which JSON carries as base64 strings.
	jsonBase64Type = "base64"
)

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// publicJSONDecodeError rewrites the encoding/json diagnostic inside err so the
// message never depends on server type or struct names. Context added around
// the diagnostic is preserved; other errors are unchanged.
func publicJSONDecodeError(err error) error {
	if err == nil {
		return nil
	}
	public, leaf := jsonDecodeDiagnostic(err)
	if leaf == nil {
		return err
	}
	// An error that already chose its own message does not repeat the
	// diagnostic and stays unchanged.
	message, raw := err.Error(), leaf.Error()
	switch {
	case strings.HasSuffix(message, raw):
		message = strings.TrimSuffix(message, raw) + public
	case strings.Contains(message, raw):
		message = strings.Replace(message, raw, public, 1)
	default:
		return err
	}
	return &jsonDecodeError{message: message, cause: err}
}

// jsonDecodeDiagnostic returns the public description of the encoding/json
// diagnostic inside err and that diagnostic, or a nil diagnostic when err has
// none.
func jsonDecodeDiagnostic(err error) (string, error) {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return jsonTypeErrorMessage(typeErr), typeErr
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		if syntaxErr.Error() == jsonUnexpectedEndMessage {
			return jsonTruncatedMessage, syntaxErr
		}
		return "malformed JSON", syntaxErr
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if name, ok := strings.CutPrefix(current.Error(), jsonUnknownFieldPrefix); ok {
			return "unknown field " + name, current
		}
	}
	var timeErr *time.ParseError
	if errors.As(err, &timeErr) {
		return "timestamp must be an RFC 3339 string", timeErr
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return jsonTruncatedMessage, io.ErrUnexpectedEOF
	}
	if errors.Is(err, io.EOF) {
		return "JSON value is required", io.EOF
	}
	return "", nil
}

func jsonTypeErrorMessage(err *json.UnmarshalTypeError) string {
	subject := "value"
	if err.Field != "" {
		subject = "field " + strconv.Quote(err.Field)
	}
	received := jsonReceivedType(err.Value)
	expected := jsonExpectedType(err.Type)
	switch {
	case expected == jsonBase64Type && received == "string":
		return subject + " must be a base64-encoded string"
	case expected == jsonBase64Type:
		expected = "string"
	case received == "number" && expected == "integer":
		// A plain integer literal can only fail by exceeding the range.
		if _, literal, _ := strings.Cut(err.Value, " "); isJSONIntegerLiteral(literal) {
			return subject + " must be an integer within the supported range"
		}
		return subject + " must be an integer"
	case received == "number" && expected == "number":
		return subject + " must be a number within the supported range"
	}
	switch {
	case expected != "" && received != "":
		return subject + " must be a JSON " + expected + ", got " + received
	case expected != "":
		return subject + " must be a JSON " + expected
	default:
		return subject + " has an invalid JSON type"
	}
}

func isJSONIntegerLiteral(literal string) bool {
	digits := strings.TrimPrefix(literal, "-")
	if digits == "" {
		return false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// jsonReceivedType maps the value description reported by encoding/json
// ("string", "number 1.5", "bool", ...) to its JSON type name.
func jsonReceivedType(value string) string {
	kind, _, _ := strings.Cut(value, " ")
	switch kind {
	case "string", "number", "object", "array":
		return kind
	case "bool":
		return "boolean"
	default:
		return ""
	}
}

func jsonExpectedType(target reflect.Type) string {
	if target == nil {
		return ""
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == reflect.TypeFor[json.Number]() {
		return "number"
	}
	if target.Implements(textUnmarshalerType) || reflect.PointerTo(target).Implements(textUnmarshalerType) {
		return "string"
	}
	switch target.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Slice:
		if target.Elem().Kind() == reflect.Uint8 {
			return jsonBase64Type
		}
		return "array"
	case reflect.Array:
		return "array"
	default:
		return ""
	}
}

func decodeAgentPayload(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("operation contains trailing data")
	}
	return nil
}

// Decode only the transport envelope here. The answer owner classifies invalid
// answer Unicode, duplicate keys and control violations as answer_invalid.
type askAnswerEnvelope struct {
	Answer     json.RawMessage
	ResponseID string
}

func decodeAskAnswerRequest(r *http.Request) (askAnswerEnvelope, error) {
	var result askAnswerEnvelope
	raw, err := readRequestBody(r)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return result, badRequest(errors.New("answer request must be an object"))
	}
	seen := map[string]bool{}
	for decoder.More() {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return result, requestBodyError(err)
		}
		key, ok := token.(string)
		if !ok || seen[key] || (key != "answer" && key != "response_id") {
			return result, badRequest(errors.New("answer request contains an unknown or duplicate field"))
		}
		keyRaw := bytes.TrimSpace(raw[start:decoder.InputOffset()])
		keyRaw = bytes.TrimSpace(bytes.TrimPrefix(keyRaw, []byte(",")))
		if _, err := canonicalJSON(keyRaw); err != nil {
			return result, badRequest(err)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return result, requestBodyError(err)
		}
		if key == "answer" {
			result.Answer = value
			continue
		}
		if _, err := canonicalJSON(value); err != nil {
			return result, badRequest(err)
		}
		if err := json.Unmarshal(value, &result.ResponseID); err != nil {
			return result, badRequest(errors.New("response_id must be a string"))
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return result, badRequest(errors.New("invalid answer request"))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return result, badRequest(errors.New("answer request contains trailing data"))
	}
	if !seen["answer"] || !seen["response_id"] || result.ResponseID == "" {
		return result, badRequest(errors.New("answer and response_id are required"))
	}
	return result, nil
}

// decodeRequestForm reads OAuth form_post callbacks through the body owner.
func decodeRequestForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, requestBodyError(err)
	}
	return r.PostForm, nil
}
