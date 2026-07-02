package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type cursorPayload struct {
	Seq int64 `json:"s"`
}

func Cursor(seq int64) string {
	if seq < 0 {
		seq = 0
	}
	raw, _ := json.Marshal(cursorPayload{Seq: seq})
	return "tc1." + base64.RawURLEncoding.EncodeToString(raw)
}

func ParseCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	const prefix = "tc1."
	if len(raw) <= len(prefix) || raw[:len(prefix)] != prefix {
		return 0, fmt.Errorf("invalid cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw[len(prefix):])
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	var payload cursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil || payload.Seq < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return payload.Seq, nil
}
