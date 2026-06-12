package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

func resolvedRetryPolicy(runPolicy json.RawMessage, taskPolicy []byte) ([]byte, error) {
	raw := bytes.TrimSpace(runPolicy)
	if len(raw) == 0 {
		raw = bytes.TrimSpace(taskPolicy)
	}
	if len(raw) == 0 {
		raw = []byte("false")
	}
	return validatedRetryPolicyJSON(raw, "retry")
}

func validatedRetryPolicyJSON(raw []byte, label string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid JSON", label)
	}
	if bytes.Equal(raw, []byte("false")) {
		return []byte("false"), nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s decode failed: %w", label, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be false or an object", label)
	}
	for field := range object {
		switch field {
		case "maxAttempts", "backoff":
		default:
			return nil, fmt.Errorf("%s.%s is not supported", label, field)
		}
	}
	maxAttempts, ok := object["maxAttempts"].(float64)
	if !ok || maxAttempts != float64(int(maxAttempts)) || maxAttempts < 1 || maxAttempts > 10 {
		return nil, fmt.Errorf("%s.maxAttempts must be an integer between 1 and 10", label)
	}
	if backoff, ok := object["backoff"]; ok {
		backoffObject, ok := backoff.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.backoff must be an object", label)
		}
		for field := range backoffObject {
			switch field {
			case "minMs", "maxMs", "factor", "jitter":
			default:
				return nil, fmt.Errorf("%s.backoff.%s is not supported", label, field)
			}
		}
		for _, field := range []string{"minMs", "maxMs"} {
			if value, ok := backoffObject[field]; ok && !isPositiveIntegerJSONNumber(value) {
				return nil, fmt.Errorf("%s.backoff.%s must be a positive integer", label, field)
			}
		}
		if factor, ok := backoffObject["factor"]; ok {
			number, ok := factor.(float64)
			if !ok || !isFinite(number) || number <= 0 {
				return nil, fmt.Errorf("%s.backoff.factor must be a positive number", label)
			}
		}
		if jitter, ok := backoffObject["jitter"]; ok {
			value, ok := jitter.(string)
			if !ok || (value != "none" && value != "full") {
				return nil, fmt.Errorf("%s.backoff.jitter must be \"none\" or \"full\"", label)
			}
		}
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("%s canonicalization failed: %w", label, err)
	}
	return canonical, nil
}

func isPositiveIntegerJSONNumber(value any) bool {
	number, ok := value.(float64)
	return ok && isFinite(number) && number == float64(int64(number)) && number > 0
}

func isFinite(number float64) bool {
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func normalizedRetryPolicy(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("false"), nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte("false"), nil
	}
	return validatedRetryPolicyJSON(trimmed, "retry")
}
