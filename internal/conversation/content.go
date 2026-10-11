// Package conversation validates the portable authored content contract.
package conversation

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ContentBytes  = 256 * 1024
	ContentParts  = 64
	ProgressBytes = 8 * 1024 * 1024
	AnswerBytes   = 64 * 1024
)

var (
	ErrInvalid     = errors.New("invalid conversational content")
	ErrUnsupported = errors.New("content kind is unsupported")
	ErrLimit       = errors.New("content limit exceeded")
)

// Content returns canonical normalized Content while preserving part boundaries.
// String shorthand is permitted only at output and response boundaries.
func Content(raw json.RawMessage, shorthand bool) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	if shorthand && len(canonical) > 0 && canonical[0] == '"' {
		var text string
		if err := json.Unmarshal(canonical, &text); err != nil {
			return nil, ErrInvalid
		}
		encoded, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		canonical, err = jsoncanon.Transform(encoded)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	if len(canonical) > ContentBytes {
		return nil, fmt.Errorf("%w: Content exceeds 256 KiB", ErrLimit)
	}
	if len(canonical) == 0 || canonical[0] != '[' {
		return nil, ErrInvalid
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &parts); err != nil {
		return nil, ErrInvalid
	}
	if len(parts) > ContentParts {
		return nil, fmt.Errorf("%w: Content exceeds 64 parts", ErrLimit)
	}
	for _, part := range parts {
		var kind string
		if err := json.Unmarshal(part["type"], &kind); err != nil {
			return nil, ErrInvalid
		}
		switch kind {
		case "text":
			var value *string
			if len(part) != 2 || json.Unmarshal(part["text"], &value) != nil || value == nil {
				return nil, ErrInvalid
			}
		case "json":
			if len(part) != 2 || part["value"] == nil {
				return nil, ErrInvalid
			}
		default:
			return nil, ErrUnsupported
		}
	}
	return canonical, nil
}
