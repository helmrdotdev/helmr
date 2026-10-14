package content

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// Input is conversational text, including live messages. Part boundaries and text are preserved; there is no string shorthand.
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

// Invocation accepts finite JSON for structured work. A target's pinned contract
// is checked separately; business validators execute only in its runtime.
func Invocation(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(canonical) > ContentBytes {
		return nil, fmt.Errorf("%w: Input exceeds 256 KiB", ErrLimit)
	}
	return canonical, nil
}

func InvocationFor(kind string, raw json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case "text":
		return Input(raw)
	case "json":
		return Invocation(raw)
	default:
		return nil, ErrInvalid
	}
}
