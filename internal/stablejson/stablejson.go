package stablejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func Encode(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode stable JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != nil {
		if err == io.EOF {
			stable, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("encode stable JSON: %w", err)
			}
			return stable, nil
		}
		return nil, fmt.Errorf("decode stable JSON trailing data: %w", err)
	}
	return nil, fmt.Errorf("stable JSON contains trailing data")
}
