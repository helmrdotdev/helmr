package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type Context struct {
	TraceID     string `json:"trace_id"`
	SpanID      string `json:"span_id,omitempty"`
	Traceparent string `json:"traceparent,omitempty"`
}

func NewTraceID() (string, error) {
	return randomHex(16)
}

func NewSpanID() (string, error) {
	return randomHex(8)
}

func Traceparent(traceID string, spanID string) string {
	return "00-" + traceID + "-" + spanID + "-01"
}

func NewContext() (Context, error) {
	traceID, err := NewTraceID()
	if err != nil {
		return Context{}, err
	}
	spanID, err := NewSpanID()
	if err != nil {
		return Context{}, err
	}
	return Context{TraceID: traceID, SpanID: spanID, Traceparent: Traceparent(traceID, spanID)}, nil
}

func randomHex(size int) (string, error) {
	var value [16]byte
	if size > len(value) {
		return "", fmt.Errorf("random hex size %d exceeds max %d", size, len(value))
	}
	for {
		if _, err := rand.Read(value[:size]); err != nil {
			return "", fmt.Errorf("generate trace context: %w", err)
		}
		if !allZero(value[:size]) {
			return hex.EncodeToString(value[:size]), nil
		}
	}
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
