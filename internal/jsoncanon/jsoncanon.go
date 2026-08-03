package jsoncanon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
)

// Transform rejects ambiguous input so every digest and fingerprint binds one
// JSON meaning across languages.
func Transform(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("canonical JSON is empty")
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("canonical JSON is not valid UTF-8")
	}
	if err := validateUnicodeEscapes(raw); err != nil {
		return nil, err
	}
	if err := validateJSON(raw); err != nil {
		return nil, err
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("compact canonical JSON input: %w", err)
	}
	canonical, err := jcs.Transform(compact.Bytes())
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return canonical, nil
}

func validateJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := validateValue(decoder); err != nil {
		return fmt.Errorf("validate canonical JSON input: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("validate canonical JSON input: trailing data")
		}
		return fmt.Errorf("validate canonical JSON input: trailing data: %w", err)
	}
	return nil
}

func validateValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object name has type %T", keyToken)
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object name %q", key)
				}
				seen[key] = struct{}{}
				if err := validateValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("object ended with %v", end)
			}
		case '[':
			for decoder.More() {
				if err := validateValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("array ended with %v", end)
			}
		default:
			return fmt.Errorf("unexpected delimiter %q", value)
		}
	case json.Number:
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return fmt.Errorf("number %q is not an I-JSON double", value)
		}
	case nil, bool, string:
		return nil
	default:
		return fmt.Errorf("unexpected JSON token type %T", token)
	}
	return nil
}

func validateUnicodeEscapes(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString {
				continue
			}
			if index+1 >= len(raw) {
				return fmt.Errorf("canonical JSON string ends with an escape")
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			first, err := parseHex16(raw, index+2)
			if err != nil {
				return err
			}
			if first >= 0xdc00 && first <= 0xdfff {
				return fmt.Errorf("canonical JSON contains an unpaired low surrogate")
			}
			if first >= 0xd800 && first <= 0xdbff {
				if index+12 > len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return fmt.Errorf("canonical JSON contains an unpaired high surrogate")
				}
				second, err := parseHex16(raw, index+8)
				if err != nil {
					return err
				}
				if second < 0xdc00 || second > 0xdfff {
					return fmt.Errorf("canonical JSON high surrogate is not followed by a low surrogate")
				}
				index += 11
				continue
			}
			index += 5
		}
	}
	return nil
}

func parseHex16(raw []byte, start int) (uint16, error) {
	if start+4 > len(raw) {
		return 0, fmt.Errorf("canonical JSON contains an incomplete Unicode escape")
	}
	value, err := strconv.ParseUint(string(raw[start:start+4]), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("canonical JSON contains an invalid Unicode escape")
	}
	return uint16(value), nil
}
