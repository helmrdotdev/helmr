package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/jackc/pgx/v5"
)

type errKind int

const (
	errBadRequest errKind = iota + 1
	errUnauthorized
	errForbidden
	errNotFound
	errMethodNotAllowed
	errConflict
	errGone
	errUnprocessable
	errTooLarge
	errBadGateway
	errNotImplemented
	errUnavailable
	errTooManyRequests
)

var (
	errRecordNotFound                 = errors.New("record not found")
	errPermissionRequired             = errors.New("permission is required")
	errAPIKeyEnvironmentScopeRequired = errors.New("API key is not bound to an environment")
)

type errorCoder interface {
	ErrorCode() string
}

type errorRetryer interface {
	ErrorRetryable() bool
}

type errorDetailer interface {
	ErrorDetails() map[string]json.RawMessage
}

type staleAuthorityOperation string

const (
	staleAuthorityRunStart       staleAuthorityOperation = "run_start"
	staleAuthorityTaskCompletion staleAuthorityOperation = "task_completion"
	staleAuthorityChildTask      staleAuthorityOperation = "child_task_invoke"
)

type staleAuthorityError struct {
	operation staleAuthorityOperation
	point     string
	cause     error
}

func (e *staleAuthorityError) Error() string {
	switch e.operation {
	case staleAuthorityRunStart:
		return "run start authority is stale"
	case staleAuthorityTaskCompletion:
		return "task completion authority is stale"
	case staleAuthorityChildTask:
		return "child task invocation authority is stale"
	default:
		return "worker authority is stale"
	}
}

func (e *staleAuthorityError) Unwrap() error { return e.cause }

func (e *staleAuthorityError) ErrorCode() string {
	return string(e.operation) + "_stale"
}

func (e *staleAuthorityError) ErrorDetails() map[string]json.RawMessage {
	point, _ := json.Marshal(e.point)
	return map[string]json.RawMessage{"point": point}
}

func staleAuthority[P ~string](operation staleAuthorityOperation, point P, err error) error {
	if err == nil || point == "" {
		return err
	}
	var sentinel error
	switch operation {
	case staleAuthorityRunStart:
		sentinel = errStaleRunLeaseClaim
	case staleAuthorityTaskCompletion:
		sentinel = errStaleTaskCompletion
	case staleAuthorityChildTask:
		sentinel = errChildTaskInvokeStale
	default:
		return err
	}
	if !errors.Is(err, sentinel) {
		return err
	}
	var existing *staleAuthorityError
	if errors.As(err, &existing) {
		return err
	}
	return &staleAuthorityError{operation: operation, point: string(point), cause: err}
}

func staleAuthorityPointOf(err error) (string, bool) {
	var stale *staleAuthorityError
	if !errors.As(err, &stale) || stale.point == "" {
		return "", false
	}
	return stale.point, true
}

type codedError struct {
	code      string
	message   string
	retryable bool
}

func (e codedError) Error() string {
	if e.message != "" {
		return e.message
	}
	return e.code
}

func (e codedError) ErrorCode() string {
	return e.code
}

func (e codedError) ErrorRetryable() bool {
	return e.retryable
}

type apiError struct {
	kind errKind
	err  error
}

func (e apiError) Error() string {
	return e.err.Error()
}

func (e apiError) Unwrap() error {
	return e.err
}

func badRequest(err error) error {
	return apiError{kind: errBadRequest, err: err}
}

func unauthorized(err error) error {
	return apiError{kind: errUnauthorized, err: err}
}

func forbidden(err error) error {
	return apiError{kind: errForbidden, err: err}
}

func notFound(err error) error {
	return apiError{kind: errNotFound, err: err}
}

func conflict(err error) error {
	return apiError{kind: errConflict, err: err}
}

func gone(err error) error {
	return apiError{kind: errGone, err: err}
}

func tooLarge(err error) error {
	return apiError{kind: errTooLarge, err: err}
}

func badGateway(err error) error {
	return apiError{kind: errBadGateway, err: err}
}

func unavailable(err error) error {
	return apiError{kind: errUnavailable, err: err}
}

func tooManyRequests(err error) error {
	return apiError{kind: errTooManyRequests, err: err}
}

func errorStatus(err error) int {
	var apiErr apiError
	if !errors.As(err, &apiErr) {
		return http.StatusInternalServerError
	}
	switch apiErr.kind {
	case errBadRequest:
		return http.StatusBadRequest
	case errUnauthorized:
		return http.StatusUnauthorized
	case errForbidden:
		return http.StatusForbidden
	case errNotFound:
		return http.StatusNotFound
	case errMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case errConflict:
		return http.StatusConflict
	case errGone:
		return http.StatusGone
	case errUnprocessable:
		return http.StatusUnprocessableEntity
	case errTooLarge:
		return http.StatusRequestEntityTooLarge
	case errBadGateway:
		return http.StatusBadGateway
	case errNotImplemented:
		return http.StatusNotImplemented
	case errUnavailable:
		return http.StatusServiceUnavailable
	case errTooManyRequests:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	writeErrorStatus(w, errorStatus(err), err)
}

func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	code := defaultErrorCode(status)
	message := publicErrorMessage(status, err)
	var details map[string]json.RawMessage
	var coder errorCoder
	if errors.As(err, &coder) && coder.ErrorCode() != "" {
		code = coder.ErrorCode()
	}
	var detailer errorDetailer
	if errors.As(err, &detailer) {
		details = detailer.ErrorDetails()
	}
	writeJSON(w, status, api.HTTPErrorResponse{Error: api.HTTPError{
		Code: code, Message: message, Details: details,
	}})
}

func defaultErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusGone:
		return "gone"
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusNotImplemented:
		return "not_implemented"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "internal_error"
	}
}

func publicErrorMessage(status int, err error) string {
	if status < http.StatusInternalServerError {
		if message := err.Error(); message != "" {
			return message
		}
	}
	var coded codedError
	if errors.As(err, &coded) && coded.message != "" {
		return coded.message
	}
	switch status {
	case http.StatusBadGateway:
		return "upstream service is unavailable"
	case http.StatusNotImplemented:
		return "operation is not implemented"
	case http.StatusServiceUnavailable:
		return "service is unavailable"
	default:
		return "internal server error"
	}
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errRecordNotFound)
}
