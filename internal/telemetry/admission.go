package telemetry

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	MaxEventMessageBytes   = 4 << 10
	MaxEventPayloadBytes   = 64 << 10
	MaxRunLogContentBytes  = 192 << 10
	MaxTelemetryBatchBytes = 8 << 20
)

func ValidateEvent(message string, payload []byte) error {
	if !utf8.ValidString(message) || len(message) > MaxEventMessageBytes {
		return fmt.Errorf("telemetry event message must be valid UTF-8 no larger than %d bytes", MaxEventMessageBytes)
	}
	if len(payload) > MaxEventPayloadBytes {
		return fmt.Errorf("telemetry event payload exceeds %d bytes", MaxEventPayloadBytes)
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("telemetry event payload must be valid JSON")
	}
	return nil
}

func ValidateRunLog(content []byte) error {
	if len(content) > MaxRunLogContentBytes {
		return fmt.Errorf("telemetry run log content exceeds %d bytes", MaxRunLogContentBytes)
	}
	return nil
}
