package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	// MaxMetadataBytes bounds a Run's canonical metadata object.
	MaxMetadataBytes = 256 << 10
	// maxMetadataKeyBytes bounds one metadata key.
	maxMetadataKeyBytes = 512
)

// NormalizeMetadata canonicalizes a metadata object no larger than limit
// bytes; empty metadata is the empty object. Its errors name the metadata by
// label.
func NormalizeMetadata(raw json.RawMessage, limit int, label string) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !jsonObject(canonical) {
		return nil, fmt.Errorf("%s metadata must be an unambiguous JSON object", label)
	}
	if len(canonical) > limit {
		return nil, fmt.Errorf("%s metadata exceeds %d bytes", label, limit)
	}
	return canonical, nil
}

func jsonObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// MetadataMutation is a validated mutation of a Run's metadata: set one key,
// patch several keys, or increment one numeric key.
type MetadataMutation struct {
	operation string
	key       string
	value     json.RawMessage
	patch     map[string]json.RawMessage
	amount    *float64
	canonical json.RawMessage
}

// NewMetadataMutation validates a metadata mutation. A set takes only key and
// value, a patch only patch, and an increment only key and a finite amount.
func NewMetadataMutation(operation, key string, value, patch json.RawMessage, amount *float64) (MetadataMutation, error) {
	mutation := MetadataMutation{operation: operation}
	switch operation {
	case "set":
		if err := validateMetadataKey(key); err != nil {
			return MetadataMutation{}, err
		}
		if len(value) == 0 || len(patch) != 0 || amount != nil {
			return MetadataMutation{}, errors.New("set requires only key and value")
		}
		canonical, err := jsoncanon.Transform(value)
		if err != nil {
			return MetadataMutation{}, fmt.Errorf("set value is invalid: %w", err)
		}
		mutation.key = key
		mutation.value = canonical
	case "patch":
		if key != "" || len(value) != 0 || amount != nil {
			return MetadataMutation{}, errors.New("patch requires only patch")
		}
		normalized, err := NormalizeMetadata(patch, MaxMetadataBytes, "run metadata patch")
		if err != nil {
			return MetadataMutation{}, err
		}
		if err := json.Unmarshal(normalized, &mutation.patch); err != nil {
			return MetadataMutation{}, err
		}
		for key := range mutation.patch {
			if err := validateMetadataKey(key); err != nil {
				return MetadataMutation{}, err
			}
		}
	case "increment":
		if err := validateMetadataKey(key); err != nil {
			return MetadataMutation{}, err
		}
		if len(value) != 0 || len(patch) != 0 ||
			amount == nil || math.IsNaN(*amount) || math.IsInf(*amount, 0) {
			return MetadataMutation{}, errors.New("increment requires only key and a finite amount")
		}
		mutation.key = key
		increment := *amount
		mutation.amount = &increment
	default:
		return MetadataMutation{}, errors.New("operation must be set, patch, or increment")
	}
	encoded, err := json.Marshal(map[string]any{
		"operation": mutation.operation, "key": mutation.key,
		"value": mutation.value, "patch": mutation.patch, "amount": mutation.amount,
	})
	if err != nil {
		return MetadataMutation{}, err
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		return MetadataMutation{}, err
	}
	mutation.canonical = canonical
	return mutation, nil
}

// Operation is the mutation's operation: set, patch or increment.
func (m MetadataMutation) Operation() string { return m.operation }

// Key is the key a set or increment addresses; it is empty for a patch.
func (m MetadataMutation) Key() string { return m.key }

// apply returns the metadata the mutation makes of current, normalized.
func (m MetadataMutation) apply(current json.RawMessage) (json.RawMessage, error) {
	values := make(map[string]json.RawMessage)
	if len(current) != 0 {
		if err := json.Unmarshal(current, &values); err != nil {
			return nil, fmt.Errorf("stored run metadata is invalid: %w", err)
		}
	}
	switch m.operation {
	case "set":
		values[m.key] = m.value
	case "patch":
		maps.Copy(values, m.patch)
	case "increment":
		currentValue := float64(0)
		if raw, ok := values[m.key]; ok {
			if err := json.Unmarshal(raw, &currentValue); err != nil ||
				math.IsNaN(currentValue) || math.IsInf(currentValue, 0) {
				return nil, fmt.Errorf("run metadata key %q is not a finite number", m.key)
			}
		}
		next := currentValue + *m.amount
		if math.IsNaN(next) || math.IsInf(next, 0) {
			return nil, fmt.Errorf("run metadata increment for key %q is not finite", m.key)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		values[m.key] = raw
	default:
		return nil, errors.New("run metadata mutation is invalid")
	}
	next, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return NormalizeMetadata(next, MaxMetadataBytes, "run")
}

func validateMetadataKey(value string) error {
	if value == "" || !utf8.ValidString(value) || len([]byte(value)) > maxMetadataKeyBytes {
		return fmt.Errorf("metadata key must be nonempty UTF-8 no larger than %d bytes", maxMetadataKeyBytes)
	}
	return nil
}
