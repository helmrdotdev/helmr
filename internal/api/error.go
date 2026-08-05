package api

import "encoding/json"

// HTTPError is the public error value returned when an HTTP operation fails.
type HTTPError struct {
	Code    string                     `json:"code"`
	Message string                     `json:"message"`
	Details map[string]json.RawMessage `json:"details,omitempty"`
}

// HTTPErrorResponse is the common envelope for Helmr-owned HTTP errors.
type HTTPErrorResponse struct {
	Error HTTPError `json:"error"`
}
