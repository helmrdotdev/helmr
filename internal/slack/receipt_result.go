package slack

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// One budget covers receipt commit and any synchronous modal opening, including
// network time. Execution and projection happen after durable receipt admission.
const receiptTimeout = 2500 * time.Millisecond

type receiptDisposition uint8

const (
	receiptAccepted receiptDisposition = iota
	receiptInvalid
	receiptUnauthorized
	receiptConflict
	receiptRetryable
)

type receiptResult struct {
	disposition receiptDisposition
	payload     json.RawMessage
	err         error
}

func receiptOutcome(err error) receiptResult {
	d := receiptAccepted
	switch {
	case err == nil:
	case errors.Is(err, errGestureConflict):
		d = receiptConflict
	case errors.Is(err, errControlInvalid), errors.Is(err, errGestureInvalid):
		d = receiptInvalid
	default:
		d = receiptRetryable
	}
	return receiptResult{disposition: d, err: err}
}

func (r receiptResult) writeHTTP(w http.ResponseWriter) {
	status := http.StatusOK
	switch r.disposition {
	case receiptInvalid:
		status = http.StatusBadRequest
	case receiptUnauthorized:
		status = http.StatusUnauthorized
	case receiptConflict:
		status = http.StatusConflict
	case receiptRetryable:
		status = http.StatusServiceUnavailable
	}
	if len(r.payload) > 0 && status == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(r.payload)
		return
	}
	w.WriteHeader(status)
}
