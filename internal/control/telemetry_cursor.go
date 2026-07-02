package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const telemetryCursorPrefix = "tc1."

type telemetryCursorPayload struct {
	Seq int64 `json:"s"`
}

func telemetryCursor(seq int64) string {
	payload, err := json.Marshal(telemetryCursorPayload{Seq: seq})
	if err != nil {
		return ""
	}
	return telemetryCursorPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func parseTelemetryCursor(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	encoded, ok := strings.CutPrefix(value, telemetryCursorPrefix)
	if !ok || encoded == "" {
		return 0, errors.New("cursor must be an opaque telemetry cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, errors.New("cursor must be an opaque telemetry cursor")
	}
	var payload telemetryCursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil || payload.Seq < 0 {
		return 0, errors.New("cursor must be an opaque telemetry cursor")
	}
	return payload.Seq, nil
}
