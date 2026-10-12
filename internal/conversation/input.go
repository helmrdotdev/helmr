package conversation

import (
	"encoding/json"
)

// Input is the single authored input contract for starts, queued work and live
// messages. Part boundaries and text are preserved; there is no string shorthand.
func Input(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := Content(raw, false)
	if err != nil {
		return nil, err
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(canonical, &parts); err != nil {
		return nil, ErrInvalid
	}
	for _, part := range parts {
		if part.Type != "text" {
			return nil, ErrUnsupported
		}
	}
	return canonical, nil
}
