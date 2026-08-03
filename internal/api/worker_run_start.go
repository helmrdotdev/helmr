package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func (request *WorkerRunStartRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*request = WorkerRunStartRequest{}
	for name := range fields {
		switch name {
		case "lease", "fresh", "restore", "attach":
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
		request.Fresh = &WorkerRunStartFresh{}
		if err := decodeStrictJSON(raw, request.Fresh); err != nil {
			return fmt.Errorf("fresh: %w", err)
		}
	}
	if raw, present := fields["restore"]; present {
		arms++
		if isStartJSONNull(raw) {
			return errors.New("restore must not be null")
		}
		request.Restore = &WorkerRunStartRestore{}
		if err := decodeStrictJSON(raw, request.Restore); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	if raw, present := fields["attach"]; present {
		arms++
		if isStartJSONNull(raw) {
			return errors.New("attach must not be null")
		}
		attach, err := decodeWorkerRunStartAttach(raw)
		if err != nil {
			return err
		}
		request.Attach = attach
	}
	if arms != 1 {
		return errors.New("exactly one of fresh, restore, or attach is required")
	}
	return nil
}

func decodeWorkerRunStartAttach(data []byte) (*WorkerRunStartAttach, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	for name := range fields {
		if name != "child" && name != "parent" {
			return nil, fmt.Errorf("attach: unknown field %q", name)
		}
	}
	attach := &WorkerRunStartAttach{}
	arms := 0
	if raw, present := fields["child"]; present {
		arms++
		if isStartJSONNull(raw) {
			return nil, errors.New("attach.child must not be null")
		}
		attach.Child = &WorkerRunStartChildAttach{}
		if err := decodeStrictJSON(raw, attach.Child); err != nil {
			return nil, fmt.Errorf("attach.child: %w", err)
		}
	}
	if raw, present := fields["parent"]; present {
		arms++
		if isStartJSONNull(raw) {
			return nil, errors.New("attach.parent must not be null")
		}
		attach.Parent = &WorkerRunStartParentAttach{}
		if err := decodeStrictJSON(raw, attach.Parent); err != nil {
			return nil, fmt.Errorf("attach.parent: %w", err)
		}
	}
	if arms != 1 {
		return nil, errors.New("attach must contain exactly one of child or parent")
	}
	return attach, nil
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
