package executor

import (
	"errors"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func runtimeOperationFailure(
	err error,
	fallbackCode string,
	fallbackMessage string,
) (workerapi.RuntimeOperationFailure, bool) {
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) || !semanticRuntimeHTTPError(httpErr) {
		return workerapi.RuntimeOperationFailure{}, false
	}
	code := runtimeOperationCode(httpErr, fallbackCode)
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = fallbackMessage
	}
	return workerapi.RuntimeOperationFailure{
		Code: code, Message: message, Retryable: runtimeOperationRetryable(code),
	}, true
}

func semanticRuntimeHTTPError(err *httpclient.Error) bool {
	switch err.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	case http.StatusConflict:
		return runtimeOperationCode(err, "") != ""
	default:
		return false
	}
}

func runtimeOperationCode(err *httpclient.Error, fallback string) string {
	code := strings.TrimSpace(err.Code)
	switch err.StatusCode {
	case http.StatusBadRequest:
		if code == "bad_request" {
			code = ""
		}
	case http.StatusConflict:
		if code == "conflict" {
			code = ""
		}
	case http.StatusRequestEntityTooLarge:
		if code == "request_too_large" {
			code = ""
		}
	case http.StatusUnprocessableEntity:
		if code == "unprocessable_entity" {
			code = ""
		}
	}
	if code == "" {
		return fallback
	}
	return code
}

func runtimeOperationRetryable(code string) bool {
	switch code {
	case "workspace_busy", "workspace_unavailable":
		return true
	default:
		return false
	}
}

func isRuntimeOperationRejection(err error) bool {
	if err == nil {
		return false
	}
	_, ok := runtimeOperationFailure(err, "", "")
	return ok
}
