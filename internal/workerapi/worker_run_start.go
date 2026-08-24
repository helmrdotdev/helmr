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
		case "lease", "fresh", "restore":
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

	arms := 0
	if raw, present := fields["fresh"]; present {
		arms++
		if isStartJSONNull(raw) {
			return errors.New("fresh must not be null")
		}
		request.Fresh = &RunStartFresh{}
		if err := decodeStrictJSON(raw, request.Fresh); err != nil {
			return fmt.Errorf("fresh: %w", err)
		}
	}
	if raw, present := fields["restore"]; present {
		arms++
		if isStartJSONNull(raw) {
			return errors.New("restore must not be null")
		}
		request.Restore = &RunStartRestore{}
		if err := decodeStrictJSON(raw, request.Restore); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	if arms != 1 {
		return errors.New("exactly one of fresh or restore is required")
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
