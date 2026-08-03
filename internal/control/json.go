package control

import (
	"encoding/json"
	"fmt"
)

func normalizedJSONObject(raw json.RawMessage, label string) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", label, err)
	}
	return json.Marshal(value)
}
