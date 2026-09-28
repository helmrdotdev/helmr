package workerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func (request *RunWaitResumeAckRequest) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		Lease        json.RawMessage `json:"lease"`
		RunWaitID    string          `json:"run_wait_id"`
		CheckpointID string          `json:"checkpoint_id"`
	}
	if err := decodeClosedWorkerJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode run wait resume acknowledgement request: %w", err)
	}
	if len(envelope.Lease) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Lease), []byte("null")) {
		return errors.New("run wait resume acknowledgement lease is required")
	}
	var lease RunLeaseFence
	if err := decodeClosedWorkerJSON(envelope.Lease, &lease); err != nil {
		return fmt.Errorf("decode run wait resume acknowledgement lease: %w", err)
	}
	*request = RunWaitResumeAckRequest{
		Lease:        lease,
		RunWaitID:    envelope.RunWaitID,
		CheckpointID: envelope.CheckpointID,
	}
	return nil
}

func decodeClosedWorkerJSON(raw []byte, value any) error {
	if _, err := jsoncanon.Transform(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
