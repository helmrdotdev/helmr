package workerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func (request *RunStartRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*request = RunStartRequest{}
	for name := range fields {
		switch name {
		case "lease":
		default:
			return fmt.Errorf("unknown field %q", name)
		}
	}
	lease, ok := fields["lease"]
	if !ok || isStartJSONNull(lease) {
		return errors.New("lease is required")
	}
	if err := decodeStrictJSON(lease, &request.Lease); err != nil {
		return fmt.Errorf("lease: %w", err)
	}

	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing value")
	}
	return nil
}

func isStartJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
