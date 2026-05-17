package format

import (
	"encoding/json"
	"io"
)

func JSON(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func JSONLines[T any](w io.Writer, values []T) error {
	encoder := json.NewEncoder(w)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			return err
		}
	}
	return nil
}
