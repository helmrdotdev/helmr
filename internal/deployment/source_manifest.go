package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	maxPackageManifestSizeBytes = 16 << 20
	maxJSONNestingDepth         = 128
)

func decodePackageManifest(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > maxPackageManifestSizeBytes {
		return nil, fmt.Errorf(
			"package manifest size is outside [1,%d]",
			maxPackageManifestSizeBytes,
		)
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("package manifest is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder, "package manifest")
	if err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder, "package manifest"); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("package manifest root must be an object")
	}
	return object, nil
}

func decodeUniqueJSON(decoder *json.Decoder, label string) (any, error) {
	return decodeUniqueJSONDepth(decoder, label, 0)
}

func decodeUniqueJSONDepth(
	decoder *json.Decoder,
	label string,
	depth int,
) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", label, err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	if depth >= maxJSONNestingDepth {
		return nil, fmt.Errorf(
			"%s exceeds %d nested containers",
			label,
			maxJSONNestingDepth,
		)
	}

	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("decode %s object key: %w", label, err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("decode %s object key: expected string", label)
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("%s contains duplicate object member %q", label, key)
			}
			value, err := decodeUniqueJSONDepth(decoder, label, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			if err != nil {
				return nil, fmt.Errorf("decode %s object end: %w", label, err)
			}
			return nil, fmt.Errorf("decode %s object end: expected }", label)
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeUniqueJSONDepth(decoder, label, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			if err != nil {
				return nil, fmt.Errorf("decode %s array end: %w", label, err)
			}
			return nil, fmt.Errorf("decode %s array end: expected ]", label)
		}
		return array, nil
	default:
		return nil, fmt.Errorf("decode %s: unexpected delimiter %q", label, delimiter)
	}
}
